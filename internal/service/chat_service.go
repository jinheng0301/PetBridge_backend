package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/pkg/llm"
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

// SendInput is the body of POST /rooms/{id}/messages. Polish (text only) asks
// for every receiver's copy to be polished at the sender's cost (spec 5.7).
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
	quota   PolishQuota // nil until WithPolish: polish requests are ignored
	pq      PolishQueue
	now     func() time.Time
	log     *slog.Logger
}

// PolishQuota charges one polish to a user; *QuotaService implements it.
type PolishQuota interface {
	TakePolish(ctx context.Context, userID string, tz *string) error
}

// WithPolish turns on both polish entry points (spec 5.7).
func (s *ChatService) WithPolish(quota PolishQuota, pq PolishQueue) *ChatService {
	s.quota, s.pq = quota, pq
	return s
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

	// A replay returns the stored message before anything is charged (spec 5.2).
	// Two concurrent first sends may both pass this check; the insert below
	// keeps one message, and at most one extra polish is charged.
	if id, err := s.msgs.FindByClientMsgID(ctx, userID, in.ClientMsgID); err == nil {
		views, err := s.msgs.ViewsOf(ctx, id)
		if err != nil {
			return model.MessageView{}, fmt.Errorf("replay views: %w", err)
		}
		return views.Sender, nil
	} else if !errors.Is(err, model.ErrNotFound) {
		return model.MessageView{}, fmt.Errorf("find replay: %w", err)
	}

	res, err := s.mod.CheckOutgoing(ctx, body, srcLang)
	if err != nil {
		return model.MessageView{}, fmt.Errorf("moderate outgoing: %w", err)
	}
	if res.Verdict != ModPass {
		// B5 has no blocked state for messages; B6 decides what review/block do.
		s.log.Info("outgoing message flagged by moderation", "room_id", roomID, "user_id", userID, "verdict", int(res.Verdict))
	}

	id, err := uuid.NewV7()
	if err != nil {
		return model.MessageView{}, fmt.Errorf("message id: %w", err)
	}
	polishOn, err := s.senderPolish(ctx, userID, prof.TZ, in, body)
	if err != nil {
		return model.MessageView{}, err
	}
	m := model.NewMessage{ID: id.String(), RoomID: roomID, SenderID: userID, Scope: in.Scope, ToUserID: toUser,
		Kind: in.Kind, PhraseID: phraseID, SrcLang: srcLang, Body: body, ClientMsgID: in.ClientMsgID, PolishRequested: polishOn}
	var polishBy *string
	if polishOn {
		polishBy = &userID
	}
	ds := deliveriesFor(m, receivers, polishBy)

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
	if polishOn {
		for _, r := range receivers {
			if v, ok := views.Receivers[r.UserID]; ok && v.Delivery != nil &&
				v.Delivery.TranslationStatus == model.TranslationDone && v.Delivery.PolishStatus == model.PolishPending {
				s.enqueuePolish(ctx, v.Delivery.ID)
			}
		}
	}
	return views.Sender, nil
}

// senderPolish decides the sender toggle: text only, free for very short
// text, otherwise one charge to the sender. Running out of quota does not
// refuse the message (polish_requested=false tells the client why).
func (s *ChatService) senderPolish(ctx context.Context, userID string, tz *string, in SendInput, body string) (bool, error) {
	if !in.Polish || in.Kind != model.KindText || s.quota == nil {
		return false, nil
	}
	if llm.TooShortToPolish(body) {
		return true, nil
	}
	err := s.quota.TakePolish(ctx, userID, tz)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrQuotaExceeded):
		return false, nil
	default:
		return false, fmt.Errorf("polish quota: %w", err)
	}
}

// enqueuePolish hands one polish to the queue; a failure leaves it pending
// for the sweep (no in-process polishing).
func (s *ChatService) enqueuePolish(ctx context.Context, deliveryID string) {
	ectx, cancel := context.WithTimeout(ctx, enqueueTimeout)
	defer cancel()
	if err := s.pq.EnqueuePolish(ectx, deliveryID, ""); err != nil {
		s.log.Warn("polish left for the sweep: enqueue failed", "delivery_id", deliveryID, "err", err)
	}
}

// Polish is the receiver's button (POST /deliveries/{id}/polish). The row is
// claimed and the quota charged in one transaction, so repeated or concurrent
// taps charge once, and a refused charge leaves the row as it was.
func (s *ChatService) Polish(ctx context.Context, userID, deliveryID string) (model.MessageView, error) {
	w, err := s.msgs.PolishWork(ctx, deliveryID)
	if errors.Is(err, model.ErrNotFound) || (err == nil && w.ReceiverID != userID) {
		return model.MessageView{}, ErrDeliveryNotFound
	}
	if err != nil {
		return model.MessageView{}, fmt.Errorf("polish work: %w", err)
	}
	seated, err := s.members.IsActiveMember(ctx, w.RoomID, userID)
	if err != nil {
		return model.MessageView{}, fmt.Errorf("member check: %w", err)
	}
	if !seated {
		return model.MessageView{}, ErrDeliveryNotFound
	}
	if w.PolishStatus != model.PolishNone {
		return s.receiverView(ctx, w.MessageID, userID)
	}
	if w.Kind != model.KindText || w.TranslationStatus != model.TranslationDone || w.BaseText == nil || s.quota == nil {
		return model.MessageView{}, ErrPolishNotReady
	}

	prof, _, err := s.users.LoadProfile(ctx, userID, false)
	if err != nil {
		return model.MessageView{}, fmt.Errorf("load receiver: %w", err)
	}
	short := llm.TooShortToPolish(*w.BaseText)
	// The claim and the charge share one transaction. The claim's row lock
	// holds a concurrent tap until this one ends: after a commit it sees
	// pending and returns that; after a refused charge (rolled back) it finds
	// none and is charged itself. So a tap never reports a pending that is
	// later undone, and the status only moves forward.
	now := s.now()
	claimed := false
	err = s.tx.WithTx(ctx, func(ctx context.Context) error {
		ok, err := s.msgs.ClaimPolish(ctx, deliveryID, userID, now)
		if err != nil {
			return fmt.Errorf("claim polish: %w", err)
		}
		if !ok { // another tap already claimed it
			return nil
		}
		claimed = true
		if short { // nothing to rewrite: done at once, free
			if _, err := s.msgs.SetPolish(ctx, deliveryID, w.BaseText, model.PolishDone, now); err != nil {
				return fmt.Errorf("set polish: %w", err)
			}
			return nil
		}
		return s.quota.TakePolish(ctx, userID, prof.TZ)
	})
	if err != nil {
		if errors.Is(err, ErrQuotaExceeded) {
			return model.MessageView{}, err
		}
		return model.MessageView{}, fmt.Errorf("polish button: %w", err)
	}
	if claimed && !short {
		s.enqueuePolish(ctx, deliveryID)
	}
	return s.receiverView(ctx, w.MessageID, userID)
}

func (s *ChatService) receiverView(ctx context.Context, messageID, userID string) (model.MessageView, error) {
	views, err := s.msgs.ViewsOf(ctx, messageID)
	if err != nil {
		return model.MessageView{}, fmt.Errorf("message views: %w", err)
	}
	v, ok := views.Receivers[userID]
	if !ok {
		return model.MessageView{}, ErrDeliveryNotFound
	}
	return v, nil
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

// deliveriesFor decides each receiver's row: done when no AI is needed, else
// pending. With polishBy set, every row's polish is pending under polishBy,
// except a row whose base is already known and too short: that one is done
// with the base as its polished text.
func deliveriesFor(m model.NewMessage, receivers []model.Recipient, polishBy *string) []model.NewDelivery {
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
		if polishBy != nil {
			d.PolishStatus, d.PolishBy = model.PolishPending, polishBy
			if done && llm.TooShortToPolish(base) {
				b := base
				d.PolishStatus, d.PolishedText = model.PolishDone, &b
			}
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
