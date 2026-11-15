package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/jinheng0301/petbridge/internal/model"
)

var inviteCodeRE = regexp.MustCompile(`^[2-9A-HJ-NP-Z]{8}$`)

// maxRawCodeLen bounds the input NormalizeCode will look at; anything longer
// cannot be a code even after removing separators generously.
const maxRawCodeLen = 64

// NormalizeCode strips whitespace and '-' and upper-cases. ok reports whether
// the result is a well-formed code; code is "" when it is not. Raw input over
// maxRawCodeLen (64) bytes is treated as malformed without being scanned.
func NormalizeCode(raw string) (code string, ok bool) {
	if len(raw) > maxRawCodeLen {
		return "", false
	}
	c := strings.ToUpper(strings.Map(func(r rune) rune {
		if r == '-' || unicode.IsSpace(r) {
			return -1
		}
		return r
	}, raw))
	if !inviteCodeRE.MatchString(c) {
		return "", false
	}
	return c, true
}

// MaskCode is the only form in which a code may appear in logs and error
// messages: the first two characters followed by "****".
func MaskCode(code string) string {
	r := []rune(code)
	if len(r) < 2 {
		return "****"
	}
	return string(r[:2]) + "****"
}

type JoinConfig struct {
	CodeTTLDays       int
	RewardCount       int
	RewardLifetimeCap int
}

// JoinService implements join-by-code (design 5.1).
type JoinService struct {
	users   UserStore
	rooms   RoomStore
	members RoomMemberStore
	codes   InviteCodeStore
	bans    BanStore
	rewards RewardStore
	tx      TxRunner
	guard   JoinGuard
	pub     Publisher
	cfg     JoinConfig
	now     func() time.Time
	newCode func() (string, error) // tests replace it to force collisions
}

func NewJoinService(users UserStore, rooms RoomStore, members RoomMemberStore, codes InviteCodeStore,
	bans BanStore, rewards RewardStore, tx TxRunner, guard JoinGuard, pub Publisher,
	cfg JoinConfig, now func() time.Time) *JoinService {
	return &JoinService{users: users, rooms: rooms, members: members, codes: codes, bans: bans, rewards: rewards,
		tx: tx, guard: guard, pub: pub, cfg: cfg, now: now, newCode: NewCode}
}

// Join seats userID in the room of rawCode. Limits are checked before the
// transaction; CODE_INVALID outcomes count towards the cooldown and only a
// real successful join resets it. A replay neither counts nor resets (else a
// member could interleave replays with guesses and never reach the cooldown);
// everything else leaves the count alone. Events go out only after the
// transaction has committed. The post-transaction bookkeeping runs on a
// context that survives the client disconnecting: the commit already happened.
func (s *JoinService) Join(ctx context.Context, userID, ip, rawCode string) (model.JoinResult, error) {
	if d := s.guard.Cooldown(ctx, userID); d > 0 {
		return model.JoinResult{}, &CooldownError{RetryAfter: d}
	}
	if ok, retry := s.guard.Allow(ctx, ip); !ok {
		return model.JoinResult{}, &RateLimitedError{RetryAfter: retry}
	}
	code, wellFormed := NormalizeCode(rawCode)

	var res model.JoinResult
	var events []Event
	var replayed bool
	err := s.tx.WithTx(ctx, func(ctx context.Context) error {
		var err error
		res, events, replayed, err = s.joinTx(ctx, userID, code, wellFormed)
		return err
	})
	post := context.WithoutCancel(ctx)
	if err != nil {
		if errors.Is(err, ErrCodeInvalid) {
			s.guard.RecordFailure(post, userID)
		}
		return model.JoinResult{}, err
	}
	if replayed {
		return res, nil // no events either: nothing changed
	}
	s.guard.RecordSuccess(post, userID)
	for _, e := range events {
		s.pub.Publish(post, e)
	}
	return res, nil
}

// joinTx runs inside the transaction; replayed reports a repeat of an
// already-successful join. Lock order (global users -> invite_codes -> rooms
// -> room_members); B3b dissolve and code regeneration must take locks in the
// same order:
//  1. users(me) FOR NO KEY UPDATE. Not FOR UPDATE: inserting invite_edges
//     takes a FOR KEY SHARE lock on the inviter's users row through the
//     inviter_id foreign key, which FOR UPDATE conflicts with. Two users
//     joining each other's rooms at once deadlocked on exactly that.
//  2. peek the code's room without a lock, then lock ALL unused codes of that
//     room in code order, then the code itself. Taking the whole set first
//     means a joiner who fills the room and voids the remaining codes never
//     waits on another joiner holding one of them, who would in turn be
//     waiting for the room lock (the last-seat deadlock).
//  3. the target room and my own room together, ascending id.
func (s *JoinService) joinTx(ctx context.Context, userID, code string, wellFormed bool) (res model.JoinResult, events []Event, replayed bool, err error) {
	p, err := profileOf(s.users.LoadProfile(ctx, userID, true))
	if err != nil {
		return model.JoinResult{}, nil, false, err
	}
	if !p.Onboarded {
		return model.JoinResult{}, nil, false, ErrOnboardingRequired
	}
	if !wellFormed {
		return model.JoinResult{}, nil, false, ErrCodeInvalid
	}
	c, err := s.lockCode(ctx, code)
	if err != nil {
		return model.JoinResult{}, nil, false, err
	}
	now := s.now()
	if !c.ExpiresAt.After(now) {
		return model.JoinResult{}, nil, false, ErrCodeInvalid
	}
	switch c.Status {
	case "unused":
	case "used":
		if c.UsedBy == nil || *c.UsedBy != userID {
			return model.JoinResult{}, nil, false, ErrCodeInvalid
		}
		res, err := s.replay(ctx, userID, c)
		return res, nil, err == nil, err
	default: // expired, void
		return model.JoinResult{}, nil, false, ErrCodeInvalid
	}

	own, err := s.rooms.GetByOwner(ctx, userID)
	if err != nil {
		return model.JoinResult{}, nil, false, fmt.Errorf("load own room: %w", err)
	}
	locked, err := s.rooms.LockRooms(ctx, c.RoomID, own.ID)
	if err != nil {
		return model.JoinResult{}, nil, false, err
	}
	target, ok := locked[c.RoomID]
	if !ok {
		return model.JoinResult{}, nil, false, ErrCodeInvalid
	}
	mine, ok := locked[own.ID]
	if !ok {
		return model.JoinResult{}, nil, false, errors.New("own room vanished while locking")
	}
	if err := s.checkEligible(ctx, userID, deref(p.AgeBand), target); err != nil {
		return model.JoinResult{}, nil, false, err
	}

	memberCount, admitted, err := s.rooms.Admit(ctx, target.ID)
	if err != nil {
		return model.JoinResult{}, nil, false, err
	}
	if !admitted {
		return model.JoinResult{}, nil, false, ErrRoomFull
	}
	seat, err := s.members.AddMember(ctx, target.ID, userID)
	if err != nil {
		return model.JoinResult{}, nil, false, err
	}
	if err := s.codes.MarkUsed(ctx, c.Code, userID, now); err != nil {
		return model.JoinResult{}, nil, false, err
	}
	inviter := target.OwnerID // seeded zero codes have no holder; the room owner invited
	if c.IssuedTo != nil {
		inviter = *c.IssuedTo
	}
	if err := s.rewards.InsertEdge(ctx, inviter, userID, c.Code); err != nil {
		return model.JoinResult{}, nil, false, err
	}
	rewards, err := s.grantRewards(ctx, userID, target.ID, mine, c.Code, now)
	if err != nil {
		return model.JoinResult{}, nil, false, err
	}

	status := target.Status
	var holders []string
	if memberCount >= target.Capacity {
		if err := s.rooms.MarkFull(ctx, target.ID); err != nil {
			return model.JoinResult{}, nil, false, err
		}
		status = "full"
		if holders, err = s.codes.VoidUnusedForRoom(ctx, target.ID); err != nil {
			return model.JoinResult{}, nil, false, err
		}
	}
	members, err := s.members.ListMembers(ctx, target.ID)
	if err != nil {
		return model.JoinResult{}, nil, false, err
	}
	res = model.JoinResult{
		Room:    model.RoomSummary{ID: target.ID, Status: status, MemberCount: memberCount, Capacity: target.Capacity},
		SeatNo:  seat,
		Members: members,
		Rewards: rewards,
	}
	return res, joinEvents(target.ID, userID, members, holders), false, nil
}

// lockCode takes the invite_codes locks of step 2. An unknown code is ErrCodeInvalid.
func (s *JoinService) lockCode(ctx context.Context, code string) (model.InviteCode, error) {
	roomID, err := s.codes.RoomIDOf(ctx, code)
	if errors.Is(err, model.ErrNotFound) {
		return model.InviteCode{}, ErrCodeInvalid
	}
	if err != nil {
		return model.InviteCode{}, err
	}
	if err := s.codes.LockUnusedForRoom(ctx, roomID); err != nil {
		return model.InviteCode{}, err
	}
	c, err := s.codes.LockByCode(ctx, code)
	if errors.Is(err, model.ErrNotFound) {
		return model.InviteCode{}, ErrCodeInvalid
	}
	return c, err
}

// checkEligible applies the business checks in priority order (design 5.1 step 3).
func (s *JoinService) checkEligible(ctx context.Context, userID, ageBand string, target model.RoomState) error {
	if target.Status == "closed" || target.OwnerID == userID {
		return ErrCodeInvalid
	}
	banned, err := s.bans.IsBanned(ctx, target.ID, userID)
	if err != nil {
		return err
	}
	if banned {
		return ErrCodeInvalid
	}
	member, err := s.members.IsActiveMember(ctx, target.ID, userID)
	if err != nil {
		return err
	}
	if member {
		return &AlreadyMemberError{RoomID: target.ID}
	}
	n, err := s.members.CountActiveForUser(ctx, userID)
	if err != nil {
		return err
	}
	if n >= 2 {
		return ErrRoomLimit
	}
	bands, err := s.members.AgeBands(ctx, target.ID)
	if err != nil {
		return err
	}
	for _, b := range bands {
		if b != ageBand {
			return ErrAgeRestricted
		}
	}
	return nil
}

// replay answers a repeated join with a code the caller already used. It is
// only a replay while the caller still sits in that room; otherwise (left,
// removed, dissolved) the code is simply invalid, so a removed user cannot
// learn of the removal by replaying.
func (s *JoinService) replay(ctx context.Context, userID string, c model.InviteCode) (model.JoinResult, error) {
	locked, err := s.rooms.LockRooms(ctx, c.RoomID)
	if err != nil {
		return model.JoinResult{}, err
	}
	room, ok := locked[c.RoomID]
	if !ok {
		return model.JoinResult{}, ErrCodeInvalid
	}
	members, err := s.members.ListMembers(ctx, room.ID)
	if err != nil {
		return model.JoinResult{}, err
	}
	seat := 0
	for _, m := range members {
		if m.UserID == userID {
			seat = m.SeatNo
		}
	}
	if seat == 0 {
		return model.JoinResult{}, ErrCodeInvalid
	}
	rewards, err := s.codes.RewardsFor(ctx, c.Code)
	if err != nil {
		return model.JoinResult{}, err
	}
	out := make([]model.CodeInfo, len(rewards))
	for i, r := range rewards {
		out[i] = model.CodeInfo{Code: r.Code, ExpiresAt: r.ExpiresAt.UTC()}
	}
	return model.JoinResult{
		Room:    model.RoomSummary{ID: room.ID, Status: room.Status, MemberCount: room.MemberCount, Capacity: room.Capacity},
		SeatNo:  seat,
		Members: members,
		Rewards: out,
	}, nil
}
