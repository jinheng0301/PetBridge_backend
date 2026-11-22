package service

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/jinheng0301/petbridge/internal/model"
)

// Message limits (spec 5.1, 5.2).
const (
	maxTextRunes = 500
	adultBand    = "18_plus"
)

// enqueueTimeout bounds the hand-off to the queue: a hung Redis must not hold
// the send request; past it the message is translated inline instead.
const enqueueTimeout = 1 * time.Second

var clientMsgIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// SendInput is the body of POST /rooms/{id}/messages. Polish is accepted but
// ignored until B5b; polish_requested is always false in B5a.
type SendInput struct {
	ClientMsgID, Kind, Text, PhraseID, Scope, ToUserID string
	Polish                                             bool
}

// ChatService sends and lists room messages (spec 5.2, 5.3).
type ChatService struct {
	tx      TxRunner
	users   UserStore
	members RoomMemberStore
	msgs    MessageStore
	mod     Moderator
	q       TranslateQueue
	inline  *InlinePool
	pub     Publisher
	now     func() time.Time
	log     *slog.Logger
}

func NewChatService(tx TxRunner, users UserStore, members RoomMemberStore, msgs MessageStore, mod Moderator,
	q TranslateQueue, inline *InlinePool, pub Publisher, now func() time.Time, log *slog.Logger) *ChatService {
	return &ChatService{tx: tx, users: users, members: members, msgs: msgs, mod: mod, q: q, inline: inline,
		pub: pub, now: now, log: log}
}

// Send stores the message with one delivery per receiver and returns the sender's view.
// A replayed client_msg_id returns the stored message without events or enqueue.
func (s *ChatService) Send(ctx context.Context, userID, roomID string, in SendInput) (model.MessageView, error) {
	body, err := validateSend(&in)
	if err != nil {
		return model.MessageView{}, err
	}

	seated, err := s.members.IsActiveMember(ctx, roomID, userID)
	if err != nil {
		return model.MessageView{}, fmt.Errorf("member check: %w", err)
	}
	if !seated {
		return model.MessageView{}, ErrRoomNotFound
	}
	prof, _, err := s.users.LoadProfile(ctx, userID, false)
	if err != nil {
		return model.MessageView{}, fmt.Errorf("load sender: %w", err)
	}
	srcLang, senderBand := deref(prof.NativeLang), deref(prof.AgeBand)

	all, err := s.msgs.ActiveRecipients(ctx, roomID)
	if err != nil {
		return model.MessageView{}, fmt.Errorf("recipients: %w", err)
	}
	var receivers []model.Recipient
	var toUser *string
	if in.Scope == model.ScopeDM {
		var peer *model.Recipient
		for i := range all {
			if all[i].UserID == in.ToUserID && in.ToUserID != userID {
				peer = &all[i]
				break
			}
		}
		if peer == nil {
			return model.MessageView{}, &ValidationError{Fields: []string{"to_user_id"}}
		}
		if senderBand != adultBand || peer.AgeBand != adultBand {
			return model.MessageView{}, ErrAgeRestricted
		}
		receivers = []model.Recipient{*peer}
		toUser = &peer.UserID
	} else {
		for _, r := range all {
			if r.UserID != userID {
				receivers = append(receivers, r)
			}
		}
	}

	var phraseID *string
	if in.Kind == model.KindPhrase {
		phraseID = &in.PhraseID
		if t, ok := PhraseText(in.PhraseID, srcLang); ok {
			body = t
		}
	}

	verdict, err := s.mod.CheckOutgoing(ctx, body, srcLang)
	if err != nil {
		return model.MessageView{}, fmt.Errorf("moderate outgoing: %w", err)
	}
	if verdict != ModPass {
		// B5 has no blocked state for messages; B6 decides what review/block do.
		s.log.Info("outgoing message flagged by moderation", "room_id", roomID, "user_id", userID, "verdict", int(verdict))
	}

	id, err := uuid.NewV7()
	if err != nil {
		return model.MessageView{}, fmt.Errorf("message id: %w", err)
	}
	m := model.NewMessage{ID: id.String(), RoomID: roomID, SenderID: userID, Scope: in.Scope, ToUserID: toUser,
		Kind: in.Kind, PhraseID: phraseID, SrcLang: srcLang, Body: body, ClientMsgID: in.ClientMsgID}
	ds := deliveriesFor(m, receivers)

	var replayID string
	err = s.tx.WithTx(ctx, func(ctx context.Context) error {
		inserted, err := s.msgs.InsertMessage(ctx, m)
		if err != nil {
			return fmt.Errorf("insert message: %w", err)
		}
		if !inserted {
			replayID, err = s.msgs.FindByClientMsgID(ctx, userID, in.ClientMsgID)
			if err != nil {
				return fmt.Errorf("find replay: %w", err)
			}
			return nil
		}
		if err := s.msgs.InsertDeliveries(ctx, m.ID, ds); err != nil {
			return fmt.Errorf("insert deliveries: %w", err)
		}
		return nil
	})
	if err != nil {
		return model.MessageView{}, err
	}

	if replayID != "" {
		views, err := s.msgs.ViewsOf(ctx, replayID)
		if err != nil {
			return model.MessageView{}, fmt.Errorf("replay views: %w", err)
		}
		return views.Sender, nil
	}

	views, err := s.msgs.ViewsOf(ctx, m.ID)
	if err != nil {
		return model.MessageView{}, fmt.Errorf("message views: %w", err)
	}
	for _, r := range receivers {
		if v, ok := views.Receivers[r.UserID]; ok {
			s.pub.Publish(ctx, messageEvent(EventMessageNew, v, r.UserID))
		}
	}
	s.pub.Publish(ctx, messageEvent(EventMessageNew, views.Sender, userID))

	if hasPending(ds) {
		ectx, cancel := context.WithTimeout(ctx, enqueueTimeout)
		err := s.q.EnqueueTranslate(ectx, m.ID, "")
		cancel()
		if err != nil {
			if !s.inline.TryRun(m.ID) {
				s.log.Warn("translation left for the sweep: enqueue failed and inline pool full",
					"message_id", m.ID, "err", err)
			}
		}
	}
	return views.Sender, nil
}

// validateSend normalises in (trimmed text, default scope) and returns the
// body of a text message. It reports the first invalid field.
func validateSend(in *SendInput) (string, error) {
	bad := func(f string) (string, error) { return "", &ValidationError{Fields: []string{f}} }
	if !clientMsgIDRE.MatchString(in.ClientMsgID) {
		return bad("client_msg_id")
	}
	if in.Kind != model.KindText && in.Kind != model.KindPhrase {
		return bad("kind")
	}
	if in.Scope == "" {
		in.Scope = model.ScopeRoom
	}
	if in.Scope != model.ScopeRoom && in.Scope != model.ScopeDM {
		return bad("scope")
	}
	if in.Kind == model.KindText {
		in.Text = strings.TrimSpace(in.Text)
		if n := utf8.RuneCountInString(in.Text); n < 1 || n > maxTextRunes {
			return bad("text")
		}
		return in.Text, nil
	}
	if _, ok := PhraseText(in.PhraseID, "en"); !ok {
		return bad("phrase_id")
	}
	return "", nil
}

// deliveriesFor decides each receiver's row: done when no AI is needed, else pending.
func deliveriesFor(m model.NewMessage, receivers []model.Recipient) []model.NewDelivery {
	trivial := m.Kind == model.KindText && nothingToTranslate(m.Body)
	ds := make([]model.NewDelivery, 0, len(receivers))
	for _, r := range receivers {
		d := model.NewDelivery{ReceiverID: r.UserID, TargetLang: r.NativeLang,
			TranslationStatus: model.TranslationPending, PolishStatus: model.PolishNone}
		var base string
		done := false
		switch {
		case m.Kind == model.KindPhrase:
			base, done = PhraseText(*m.PhraseID, r.NativeLang)
		case r.NativeLang == m.SrcLang || trivial:
			base, done = m.Body, true
		}
		if done {
			d.BaseText, d.TranslationStatus = &base, model.TranslationDone
		}
		ds = append(ds, d)
	}
	return ds
}

func hasPending(ds []model.NewDelivery) bool {
	for _, d := range ds {
		if d.TranslationStatus == model.TranslationPending {
			return true
		}
	}
	return false
}

// nothingToTranslate reports a non-empty text made only of whitespace,
// punctuation, symbols (emoji) and joiners/combining marks.
func nothingToTranslate(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsSpace(r) && !unicode.In(r, unicode.P, unicode.S, unicode.Cf, unicode.Mn, unicode.Me) {
			return false
		}
	}
	return true
}

// List returns one page of the messages the caller sent or received in the room.
func (s *ChatService) List(ctx context.Context, userID, roomID string, q model.PageQuery) (model.MessagePage, error) {
	bad := func(f string) (model.MessagePage, error) {
		return model.MessagePage{}, &ValidationError{Fields: []string{f}}
	}
	switch {
	case q.Before != "" && q.After != "":
		return bad("before")
	case q.Before != "" && !isUUID(q.Before):
		return bad("before")
	case q.After != "" && !isUUID(q.After):
		return bad("after")
	case q.Limit < 0 || q.Limit > model.MaxPageLimit:
		return bad("limit")
	}
	if q.Limit == 0 {
		q.Limit = model.DefaultPageLimit
	}
	seated, err := s.members.IsActiveMember(ctx, roomID, userID)
	if err != nil {
		return model.MessagePage{}, fmt.Errorf("member check: %w", err)
	}
	if !seated {
		return model.MessagePage{}, ErrRoomNotFound
	}
	items, more, err := s.msgs.ListForViewer(ctx, roomID, userID, q)
	if err != nil {
		return model.MessagePage{}, fmt.Errorf("list messages: %w", err)
	}
	if items == nil {
		items = []model.MessageView{}
	}
	return model.MessagePage{Items: items, HasMore: more}, nil
}

// isUUID accepts only the canonical 36-character form the repositories expect.
func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil && len(s) == 36
}
