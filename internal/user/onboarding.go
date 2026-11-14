package user

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	_ "time/tzdata" // Windows has no system zoneinfo; embed it so IANA names validate everywhere

	"github.com/jinheng0301/petbridge/internal/invite"
)

var (
	ErrNotFound        = errors.New("user not found")
	ErrAccountDisabled = errors.New("account disabled")
)

// ValidationError lists the names of the invalid request fields.
type ValidationError struct{ Fields []string }

func (e *ValidationError) Error() string {
	return "invalid fields: " + strings.Join(e.Fields, ",")
}

type OnboardingInput struct {
	NativeLang string `json:"native_lang"`
	MBTI       string `json:"mbti"`
	Nickname   string `json:"nickname"`
	AgeBand    string `json:"age_band"`
	TZ         string `json:"tz"`
}

var (
	mbtiRE      = regexp.MustCompile(`^[EI][NS][TF][JP]$`)
	nativeLangs = map[string]bool{"ja": true, "ko": true, "zh-Hant": true, "zh-Hans": true, "en": true}
	ageBands    = map[string]bool{"13_17": true, "18_plus": true}
	uuidRE      = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

const maxNicknameLen = 20 // Unicode code points

// Validate returns the names of invalid fields, in request order.
func (in OnboardingInput) Validate() []string {
	var bad []string
	if !nativeLangs[in.NativeLang] {
		bad = append(bad, "native_lang")
	}
	if !mbtiRE.MatchString(in.MBTI) {
		bad = append(bad, "mbti")
	}
	if !validNickname(strings.TrimSpace(in.Nickname)) {
		bad = append(bad, "nickname")
	}
	if !ageBands[in.AgeBand] {
		bad = append(bad, "age_band")
	}
	if !validTZ(in.TZ) {
		bad = append(bad, "tz")
	}
	return bad
}

func validNickname(n string) bool {
	if n == "" || utf8.RuneCountInString(n) > maxNicknameLen {
		return false
	}
	for _, r := range n {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// validTZ rejects "" and "Local", which time.LoadLocation accepts.
func validTZ(tz string) bool {
	if tz == "" || tz == "Local" {
		return false
	}
	_, err := time.LoadLocation(tz)
	return err == nil
}

type Profile struct {
	ID         string  `json:"id"`
	Nickname   *string `json:"nickname"`
	NativeLang *string `json:"native_lang"`
	MBTI       *string `json:"mbti"`
	PetType    *string `json:"pet_type"`
	AgeBand    *string `json:"age_band"`
	TZ         *string `json:"tz"`
	Onboarded  bool    `json:"onboarded"`
}

type PetInfo struct {
	Type string `json:"type"`
	MBTI string `json:"mbti"`
}

type RoomInfo struct {
	ID          string `json:"id"`
	MemberCount int    `json:"member_count"`
	Capacity    int    `json:"capacity"`
}

type CodeInfo struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
}

type OnboardingResult struct {
	User       Profile  `json:"user"`
	Pet        PetInfo  `json:"pet"`
	Room       RoomInfo `json:"room"`
	InviteCode CodeInfo `json:"invite_code"`
}

type Service struct {
	pool        *pgxpool.Pool
	codeTTLDays int
	now         func() time.Time
	newCode     func() (string, error) // tests replace it to force collisions
}

func NewService(pool *pgxpool.Pool, codeTTLDays int, now func() time.Time) *Service {
	return &Service{pool: pool, codeTTLDays: codeTTLDays, now: now, newCode: invite.NewCode}
}

const maxCodeAttempts = 5

const profileCols = `id::text, nickname, native_lang, mbti, pet_type, age_band, tz, onboarded_at is not null, status`

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func loadProfile(ctx context.Context, q rowQuerier, userID string, lock bool) (Profile, error) {
	if !uuidRE.MatchString(userID) { // keeps the id index usable and avoids a cast error
		return Profile{}, ErrNotFound
	}
	var p Profile
	var status string
	sql := `select ` + profileCols + ` from users where id=$1`
	if lock {
		sql += ` for update`
	}
	err := q.QueryRow(ctx, sql, userID).Scan(&p.ID, &p.Nickname, &p.NativeLang, &p.MBTI, &p.PetType, &p.AgeBand, &p.TZ, &p.Onboarded, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, ErrNotFound
	}
	if err != nil {
		return Profile{}, err
	}
	if status != "active" {
		return Profile{}, ErrAccountDisabled
	}
	return p, nil
}

// Me returns the caller's profile.
func (s *Service) Me(ctx context.Context, userID string) (Profile, error) {
	return loadProfile(ctx, s.pool, userID, false)
}

// Onboard completes onboarding in one transaction (profile, pet, own room,
// owner seat, signup code). A second call returns the existing result and
// ignores its input.
func (s *Service) Onboard(ctx context.Context, userID string, in OnboardingInput) (OnboardingResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return OnboardingResult{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// The row lock serialises concurrent onboarding of the same user.
	p, err := loadProfile(ctx, tx, userID, true)
	if err != nil {
		return OnboardingResult{}, err
	}
	if p.Onboarded {
		return s.existing(ctx, tx, p)
	}
	if bad := in.Validate(); len(bad) > 0 {
		return OnboardingResult{}, &ValidationError{Fields: bad}
	}
	in.Nickname = strings.TrimSpace(in.Nickname)
	pet, _ := PetForMBTI(in.MBTI) // MBTI already validated

	now := s.now()
	if _, err := tx.Exec(ctx, `update users set nickname=$2, native_lang=$3, mbti=$4, pet_type=$5, age_band=$6, tz=$7, onboarded_at=$8
		where id=$1`, userID, in.Nickname, in.NativeLang, in.MBTI, pet, in.AgeBand, in.TZ, now); err != nil {
		return OnboardingResult{}, err
	}
	var room RoomInfo
	if err := tx.QueryRow(ctx, `insert into rooms(owner_id, member_count) values($1,1) returning id::text, member_count, capacity`,
		userID).Scan(&room.ID, &room.MemberCount, &room.Capacity); err != nil {
		return OnboardingResult{}, err
	}
	if _, err := tx.Exec(ctx, `insert into room_members(room_id, user_id, seat_no, role) values($1,$2,1,'owner')`, room.ID, userID); err != nil {
		return OnboardingResult{}, err
	}
	code, expires, err := s.insertSignupCode(ctx, tx, room.ID, userID, now)
	if err != nil {
		return OnboardingResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return OnboardingResult{}, err
	}
	p, _ = loadProfile(ctx, s.pool, userID, false)
	return OnboardingResult{User: p, Pet: PetInfo{Type: pet, MBTI: in.MBTI}, Room: room, InviteCode: CodeInfo{Code: code, ExpiresAt: expires}}, nil
}

// insertSignupCode retries on primary-key collisions. ON CONFLICT DO NOTHING
// is used because a failed INSERT would abort the surrounding transaction.
func (s *Service) insertSignupCode(ctx context.Context, tx pgx.Tx, roomID, userID string, now time.Time) (string, time.Time, error) {
	expires := now.UTC().Add(time.Duration(s.codeTTLDays) * 24 * time.Hour)
	for i := 0; i < maxCodeAttempts; i++ {
		code, err := s.newCode()
		if err != nil {
			return "", time.Time{}, err
		}
		tag, err := tx.Exec(ctx, `insert into invite_codes(code, room_id, issued_to, source, expires_at)
			values($1,$2,$3,'signup',$4) on conflict (code) do nothing`, code, roomID, userID, expires)
		if err != nil {
			return "", time.Time{}, err
		}
		if tag.RowsAffected() == 1 {
			return code, expires, nil
		}
	}
	return "", time.Time{}, errors.New("could not allocate a unique invite code")
}

func (s *Service) existing(ctx context.Context, tx pgx.Tx, p Profile) (OnboardingResult, error) {
	res := OnboardingResult{User: p, Pet: PetInfo{Type: deref(p.PetType), MBTI: deref(p.MBTI)}}
	if err := tx.QueryRow(ctx, `select id::text, member_count, capacity from rooms where owner_id=$1`, p.ID).
		Scan(&res.Room.ID, &res.Room.MemberCount, &res.Room.Capacity); err != nil {
		return OnboardingResult{}, err
	}
	if err := tx.QueryRow(ctx, `select code, expires_at from invite_codes where issued_to=$1 and source='signup'
		order by issued_at limit 1`, p.ID).Scan(&res.InviteCode.Code, &res.InviteCode.ExpiresAt); err != nil {
		return OnboardingResult{}, err
	}
	res.InviteCode.ExpiresAt = res.InviteCode.ExpiresAt.UTC() // same representation as the first response
	return res, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
