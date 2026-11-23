package service

import (
	"context"
	"time"

	"github.com/jinheng0301/petbridge/internal/model"
)

// The interfaces below are what services need from the repository layer.
// repository.* types satisfy them; unit tests may substitute fakes.

type TxRunner interface {
	WithTx(ctx context.Context, fn func(ctx context.Context) error) error
}

type UserStore interface {
	CreateEmpty(ctx context.Context) (string, error)
	State(ctx context.Context, userID string) (status string, onboarded bool, err error)
	LoadProfile(ctx context.Context, userID string, lock bool) (model.Profile, string, error)
	CompleteOnboarding(ctx context.Context, userID string, in model.OnboardingInput, pet string, at time.Time) error
}

type IdentityStore interface {
	FindUserID(ctx context.Context, provider, subject string) (string, error)
	Create(ctx context.Context, provider, subject, userID string) error
}

type RefreshTokenStore interface {
	Insert(ctx context.Context, userID, hash, deviceID string, expiresAt time.Time, family *string) (string, error)
	Rotate(ctx context.Context, hash string, now time.Time) (model.RotatedRefreshToken, error)
	LockByHash(ctx context.Context, hash string) (model.RefreshTokenState, error)
	FamilyHasLive(ctx context.Context, family string, now time.Time) (bool, error)
	RevokeFamily(ctx context.Context, family string, now time.Time) error
	SetReplacedBy(ctx context.Context, oldID, newID string) error
	RevokeFamilyOfHash(ctx context.Context, hash string, now time.Time) error
}

type RoomStore interface {
	CreateForOwner(ctx context.Context, ownerID string, capacity int) (model.RoomInfo, error)
	GetByOwner(ctx context.Context, ownerID string) (model.RoomInfo, error)
	// LockRooms dedupes ids and locks them ORDER BY id FOR UPDATE; missing ids are absent from the map.
	LockRooms(ctx context.Context, ids ...string) (map[string]model.RoomState, error)
	// Admit bumps member_count when the room is open and below capacity.
	Admit(ctx context.Context, roomID string) (memberCount int, ok bool, err error)
	MarkFull(ctx context.Context, roomID string) error
	// ListForMember returns the user's active rooms, own room first, members filled in.
	ListForMember(ctx context.Context, userID string) ([]model.RoomDetail, error)
	// Peek reads a room without locking; unknown or malformed ids return model.ErrNotFound.
	Peek(ctx context.Context, roomID string) (model.RoomState, error)
	// Vacate frees one seat (member_count-1); a full room reopens.
	Vacate(ctx context.Context, roomID string) (memberCount int, err error)
	// Reset leaves only the owner and reopens the room.
	Reset(ctx context.Context, roomID string) error
}

type RoomMemberStore interface {
	AddOwner(ctx context.Context, roomID, userID string) error
	IsActiveMember(ctx context.Context, roomID, userID string) (bool, error)
	CountActiveForUser(ctx context.Context, userID string) (int, error)
	AgeBands(ctx context.Context, roomID string) ([]string, error)
	// AddMember takes the smallest free seat (1..5) with role 'member'.
	AddMember(ctx context.Context, roomID, userID string) (seatNo int, err error)
	ListMembers(ctx context.Context, roomID string) ([]model.MemberInfo, error)
	// Leave ends the user's active seat with reason left|kicked|dissolved; left=false when not seated.
	Leave(ctx context.Context, roomID, userID, reason string, at time.Time) (left bool, err error)
	// EverMember reports any membership row, active or not.
	EverMember(ctx context.Context, roomID, userID string) (bool, error)
	// DissolveMembers ends every active non-owner seat and returns their ids by seat.
	DissolveMembers(ctx context.Context, roomID string, at time.Time) (userIDs []string, err error)
}

type InviteCodeStore interface {
	InsertSignup(ctx context.Context, code, roomID, userID string, expiresAt time.Time) (bool, error)
	FirstSignupFor(ctx context.Context, userID string) (code string, expiresAt time.Time, err error)
	// RoomIDOf reads the code's room without locking; an unknown code returns model.ErrNotFound.
	RoomIDOf(ctx context.Context, code string) (roomID string, err error)
	// LockUnusedForRoom locks the room's unused codes FOR UPDATE in code order.
	LockUnusedForRoom(ctx context.Context, roomID string) error
	// LockByCode selects FOR UPDATE; an unknown code returns model.ErrNotFound.
	LockByCode(ctx context.Context, code string) (model.InviteCode, error)
	MarkUsed(ctx context.Context, code, userID string, at time.Time) error
	// VoidUnusedForRoom voids unused codes and returns the distinct non-empty issued_to of those codes.
	VoidUnusedForRoom(ctx context.Context, roomID string) (holders []string, err error)
	// CountUnusedValid counts status=unused with expires_at > now.
	CountUnusedValid(ctx context.Context, roomID string, now time.Time) (int, error)
	// LifetimeCount counts codes issued to the user excluding source 'zero'.
	LifetimeCount(ctx context.Context, userID string) (int, error)
	InsertReward(ctx context.Context, code, roomID, userID, forCode string, expiresAt time.Time) (inserted bool, err error)
	RewardsFor(ctx context.Context, forCode string) ([]model.CodeInfo, error)
	// ListForUser reports the effective status (unused and expired -> expired), newest first.
	ListForUser(ctx context.Context, userID string, now time.Time) ([]model.CodeListItem, error)
	// InsertRegen stores an owner_regen code with an explicit issued_at; inserted=false on a code collision.
	InsertRegen(ctx context.Context, code, roomID, ownerID string, issuedAt, expiresAt time.Time) (inserted bool, err error)
	// CountRegenBetween counts the owner's owner_regen codes issued in [from, to).
	CountRegenBetween(ctx context.Context, ownerID string, from, to time.Time) (int, error)
}

type BanStore interface {
	IsBanned(ctx context.Context, roomID, userID string) (bool, error)
	Ban(ctx context.Context, roomID, userID, bannedBy string) error
}

// JoinGuard applies the join-by-code limits (design 5.3); ratelimit.JoinGuard implements it.
type JoinGuard interface {
	Cooldown(ctx context.Context, userID string) time.Duration
	Allow(ctx context.Context, ip string) (bool, time.Duration)
	RecordFailure(ctx context.Context, userID string)
	RecordSuccess(ctx context.Context, userID string)
}

// Publisher delivers realtime events. Services call it only after commit.
type Publisher interface {
	Publish(ctx context.Context, e Event)
}

type RewardStore interface {
	// Grant records the reward once per (user, room); inserted=false when already granted.
	Grant(ctx context.Context, userID, roomID string, count int) (inserted bool, err error)
	InsertEdge(ctx context.Context, inviterID, inviteeID, code string) error
}

type MessageStore interface {
	// ActiveRecipients returns every seated member of the room (the caller included) with native language and age band.
	ActiveRecipients(ctx context.Context, roomID string) ([]model.Recipient, error)
	// InsertMessage writes nothing on a (sender_id, client_msg_id) conflict and returns inserted=false.
	InsertMessage(ctx context.Context, m model.NewMessage) (inserted bool, err error)
	// FindByClientMsgID returns model.ErrNotFound when there is no such message.
	FindByClientMsgID(ctx context.Context, senderID, clientMsgID string) (messageID string, err error)
	InsertDeliveries(ctx context.Context, messageID string, ds []model.NewDelivery) error
	// ViewsOf builds the sender's view and each receiver's view; a missing message returns model.ErrNotFound.
	ViewsOf(ctx context.Context, messageID string) (model.MessageViews, error)
	// ListForViewer returns only messages the viewer sent or received, items ordered by id ascending.
	ListForViewer(ctx context.Context, roomID, viewerID string, q model.PageQuery) (items []model.MessageView, hasMore bool, err error)
	// TranslationWork reads the message and all its non-done deliveries; Pending is empty when nothing is left.
	TranslationWork(ctx context.Context, messageID string) (model.TranslationWork, error)
	// SetTranslation only touches non-done rows: writes base_text and status, translate_attempts+1, updated_at=now; changed=false means already done.
	// modStatus is the delivery's moderation verdict (model.ModOK / ModReview / ModBlock).
	SetTranslation(ctx context.Context, deliveryID string, baseText *string, status, modStatus string, now time.Time) (changed bool, err error)
	// WithdrawMessage hides the message from all receivers in one statement: messages.mod_status and every delivery's
	// mod_status become 'block', unfinished translation_status becomes 'done', pending polishes become 'fallback'.
	// It returns every receiver id.
	WithdrawMessage(ctx context.Context, messageID string, now time.Time) ([]string, error)
	// StuckTranslations returns ids (distinct) of messages whose translation must be re-run (spec 5.5 sweep, first two rules).
	StuckTranslations(ctx context.Context, now time.Time, pendingAfter, failedEvery, giveUpAfter time.Duration, limit int) ([]string, error)

	// PolishWork reads one delivery with its message and the receiver's MBTI; missing or invalid id returns model.ErrNotFound.
	PolishWork(ctx context.Context, deliveryID string) (model.PolishWork, error)
	// SetPolish only touches polish_status='pending' rows: writes polished_text, status, updated_at; changed=false means no longer pending.
	SetPolish(ctx context.Context, deliveryID string, polished *string, status string, now time.Time) (changed bool, err error)
	// ClaimPolish moves the receiver's own row from none to pending with polish_by=receiver; claimed=false when it was not none.
	ClaimPolish(ctx context.Context, deliveryID, receiverID string, now time.Time) (claimed bool, err error)
	// StuckPolishes returns ids of deliveries whose polish is pending, translation done, and not touched for pendingAfter; oldest first.
	StuckPolishes(ctx context.Context, now time.Time, pendingAfter time.Duration, limit int) ([]string, error)
	// GiveUpPolishes sets pending polishes to fallback when the translation failed and the message is older than giveUpAfter; returns the rows changed.
	GiveUpPolishes(ctx context.Context, now time.Time, giveUpAfter time.Duration, limit int) ([]model.PolishGiveUp, error)
}

// ModFlagStore writes the moderation review queue (mod_flags).
type ModFlagStore interface {
	Insert(ctx context.Context, f model.ModFlag) error
}

// BlockStore keeps user blocks.
type BlockStore interface {
	Block(ctx context.Context, blockerID, blockedID string) error // does nothing when it already exists
	Unblock(ctx context.Context, blockerID, blockedID string) error
	IsBlocking(ctx context.Context, blockerID, blockedID string) (bool, error)
	Count(ctx context.Context, blockerID string) (int, error)
	List(ctx context.Context, blockerID string) ([]model.BlockedUser, error)                   // newest first
	Between(ctx context.Context, a, b string) (bool, error)                                    // either direction
	BlockedAmong(ctx context.Context, userID string, others []string) (map[string]bool, error) // either direction
}

// ReportStore keeps user reports.
type ReportStore interface {
	// Insert returns the id and time of the already open identical report when there is one.
	Insert(ctx context.Context, r model.NewReport) (id string, createdAt time.Time, err error)
	// FindOpen returns the open report for the same (reporter, target, message_id), or model.ErrNotFound.
	FindOpen(ctx context.Context, reporterID, targetID string, messageID *string) (id string, createdAt time.Time, err error)
	CountSince(ctx context.Context, reporterID string, since time.Time) (int, error)
	// MessageReportable: the message is in roomID, sent by senderID, and reporterID has a delivery of it.
	MessageReportable(ctx context.Context, messageID, roomID, senderID, reporterID string) (bool, error)
}

// UsageStore records real AI calls per user and day (ai_usage_daily).
type UsageStore interface {
	// Add accumulates into (UTC date of day, userID, kind); calls == 0 writes nothing.
	Add(ctx context.Context, day time.Time, userID, kind string, calls int, in, out, costMicros int64) error
}
