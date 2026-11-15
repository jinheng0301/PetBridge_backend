package service

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	_ "time/tzdata" // Windows has no system zoneinfo; embed it so IANA names validate everywhere

	"github.com/jinheng0301/petbridge/internal/model"
)

var (
	mbtiRE      = regexp.MustCompile(`^[EI][NS][TF][JP]$`)
	nativeLangs = map[string]bool{"ja": true, "ko": true, "zh-Hant": true, "zh-Hans": true, "en": true}
	ageBands    = map[string]bool{"13_17": true, "18_plus": true}
)

const maxNicknameLen = 20 // Unicode code points

// ValidateOnboarding returns the names of invalid fields, in request order.
func ValidateOnboarding(in model.OnboardingInput) []string {
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

// invisibleFillers render as blank but are letters/symbols, so neither
// IsControl, Cf nor TrimSpace catches them: Hangul fillers (relevant for KR
// users) and the Braille blank. A nickname made of them looks empty.
var invisibleFillers = map[rune]bool{0x115F: true, 0x1160: true, 0x3164: true, 0xFFA0: true, 0x2800: true}

const (
	zwj = '‍' // zero-width joiner
	vs  = '️' // emoji variation selector
)

func validNickname(n string) bool {
	if n == "" || utf8.RuneCountInString(n) > maxNicknameLen {
		return false
	}
	rs := []rune(n)
	for i, r := range rs {
		switch {
		case unicode.IsControl(r), invisibleFillers[r]:
			return false
		case r == zwj:
			// Only as the glue inside an emoji sequence (family, flag, skin tone).
			// Between letters it is invisible and would let "Bob" and "Bo<ZWJ>b"
			// look identical.
			if !joinsEmoji(rs, i) {
				return false
			}
		case unicode.Is(unicode.Cf, r):
			// Bidi overrides, other zero-width characters: used to impersonate
			// other users in chat.
			return false
		}
	}
	return true
}

// joinsEmoji reports whether the ZWJ at rs[i] sits between two emoji.
func joinsEmoji(rs []rune, i int) bool {
	if i == 0 || i == len(rs)-1 {
		return false
	}
	prev, next := rs[i-1], rs[i+1]
	prevOK := unicode.Is(unicode.So, prev) || unicode.Is(unicode.Sk, prev) || prev == vs
	return prevOK && unicode.Is(unicode.So, next)
}

// validTZ rejects "" and "Local", which time.LoadLocation accepts.
func validTZ(tz string) bool {
	if tz == "" || tz == "Local" {
		return false
	}
	_, err := time.LoadLocation(tz)
	return err == nil
}

// UserService serves profiles and onboarding.
type UserService struct {
	users       UserStore
	rooms       RoomStore
	members     RoomMemberStore
	codes       InviteCodeStore
	tx          TxRunner
	codeTTLDays int
	now         func() time.Time
	newCode     func() (string, error) // tests replace it to force collisions
}

func NewUserService(users UserStore, rooms RoomStore, members RoomMemberStore, codes InviteCodeStore,
	tx TxRunner, codeTTLDays int, now func() time.Time) *UserService {
	return &UserService{users: users, rooms: rooms, members: members, codes: codes, tx: tx,
		codeTTLDays: codeTTLDays, now: now, newCode: NewCode}
}

const maxCodeAttempts = 5

// loadProfile loads a profile and applies the account-status rule.
func (s *UserService) loadProfile(ctx context.Context, userID string, lock bool) (model.Profile, error) {
	p, status, err := s.users.LoadProfile(ctx, userID, lock)
	if errors.Is(err, model.ErrNotFound) {
		return model.Profile{}, ErrUserNotFound
	}
	if err != nil {
		return model.Profile{}, err
	}
	if status != "active" {
		return model.Profile{}, ErrAccountDisabled
	}
	return p, nil
}

// AccountStatus returns users.status for the auth middleware, so a banned or
// deleted account is rejected even while its access token is still valid.
func (s *UserService) AccountStatus(ctx context.Context, userID string) (string, error) {
	status, _, err := s.users.State(ctx, userID)
	return status, err
}

// Me returns the caller's profile.
func (s *UserService) Me(ctx context.Context, userID string) (model.Profile, error) {
	return s.loadProfile(ctx, userID, false)
}

// Onboard completes onboarding in one transaction (profile, pet, own room,
// owner seat, signup code). A second call returns the existing result and
// ignores its input.
func (s *UserService) Onboard(ctx context.Context, userID string, in model.OnboardingInput) (model.OnboardingResult, error) {
	var res model.OnboardingResult
	var created bool
	err := s.tx.WithTx(ctx, func(ctx context.Context) error {
		// The row lock serialises concurrent onboarding of the same user.
		p, err := s.loadProfile(ctx, userID, true)
		if err != nil {
			return err
		}
		if p.Onboarded {
			res, err = s.existing(ctx, p)
			return err
		}
		if bad := ValidateOnboarding(in); len(bad) > 0 {
			return &ValidationError{Fields: bad}
		}
		in.Nickname = strings.TrimSpace(in.Nickname)
		pet, _ := PetForMBTI(in.MBTI) // MBTI already validated

		now := s.now()
		if err := s.users.CompleteOnboarding(ctx, userID, in, pet, now); err != nil {
			return err
		}
		room, err := s.rooms.CreateForOwner(ctx, userID)
		if err != nil {
			return err
		}
		if err := s.members.AddOwner(ctx, room.ID, userID); err != nil {
			return err
		}
		code, expires, err := s.insertSignupCode(ctx, room.ID, userID, now)
		if err != nil {
			return err
		}
		res = model.OnboardingResult{
			Pet:        model.PetInfo{Type: pet, MBTI: in.MBTI},
			Room:       room,
			InviteCode: model.CodeInfo{Code: code, ExpiresAt: expires},
		}
		created = true
		return nil
	})
	if err != nil {
		return model.OnboardingResult{}, err
	}
	if created {
		// Re-read after commit so the response carries what was actually stored.
		// The data is already committed; if the reload fails, report the error
		// so the client retries and gets the idempotent existing result,
		// instead of a 200 carrying an empty user.
		if res.User, err = s.loadProfile(ctx, userID, false); err != nil {
			return model.OnboardingResult{}, err
		}
	}
	return res, nil
}

// insertSignupCode retries on primary-key collisions.
func (s *UserService) insertSignupCode(ctx context.Context, roomID, userID string, now time.Time) (string, time.Time, error) {
	expires := now.UTC().Add(time.Duration(s.codeTTLDays) * 24 * time.Hour)
	for i := 0; i < maxCodeAttempts; i++ {
		code, err := s.newCode()
		if err != nil {
			return "", time.Time{}, err
		}
		inserted, err := s.codes.InsertSignup(ctx, code, roomID, userID, expires)
		if err != nil {
			return "", time.Time{}, err
		}
		if inserted {
			return code, expires, nil
		}
	}
	return "", time.Time{}, errors.New("could not allocate a unique invite code")
}

func (s *UserService) existing(ctx context.Context, p model.Profile) (model.OnboardingResult, error) {
	res := model.OnboardingResult{User: p, Pet: model.PetInfo{Type: deref(p.PetType), MBTI: deref(p.MBTI)}}
	room, err := s.rooms.GetByOwner(ctx, p.ID)
	if err != nil {
		return model.OnboardingResult{}, err
	}
	res.Room = room
	code, expires, err := s.codes.FirstSignupFor(ctx, p.ID)
	if err != nil {
		return model.OnboardingResult{}, err
	}
	res.InviteCode = model.CodeInfo{Code: code, ExpiresAt: expires.UTC()} // same representation as the first response
	return res, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
