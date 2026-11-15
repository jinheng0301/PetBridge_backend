package service

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/platform/db/dbtest"
	"github.com/jinheng0301/petbridge/internal/repository"
)

func valid() model.OnboardingInput {
	return model.OnboardingInput{NativeLang: "ja", MBTI: "ENFP", Nickname: "  Mimi ", AgeBand: "18_plus", TZ: "Asia/Tokyo"}
}

func TestValidate(t *testing.T) {
	mut := func(f func(*model.OnboardingInput)) model.OnboardingInput { in := valid(); f(&in); return in }
	cases := []struct {
		name string
		in   model.OnboardingInput
		want []string
	}{
		{"valid", valid(), nil},
		{"lang fr", mut(func(i *model.OnboardingInput) { i.NativeLang = "fr" }), []string{"native_lang"}},
		{"lang empty", mut(func(i *model.OnboardingInput) { i.NativeLang = "" }), []string{"native_lang"}},
		{"mbti lower", mut(func(i *model.OnboardingInput) { i.MBTI = "enfp" }), []string{"mbti"}},
		{"mbti short", mut(func(i *model.OnboardingInput) { i.MBTI = "ENF" }), []string{"mbti"}},
		{"mbti junk", mut(func(i *model.OnboardingInput) { i.MBTI = "XXXX" }), []string{"mbti"}},
		{"age under 13", mut(func(i *model.OnboardingInput) { i.AgeBand = "12_under" }), []string{"age_band"}},
		{"age empty", mut(func(i *model.OnboardingInput) { i.AgeBand = "" }), []string{"age_band"}},
		{"tz empty", mut(func(i *model.OnboardingInput) { i.TZ = "" }), []string{"tz"}},
		{"tz Local", mut(func(i *model.OnboardingInput) { i.TZ = "Local" }), []string{"tz"}},
		{"tz unknown", mut(func(i *model.OnboardingInput) { i.TZ = "Mars/Base" }), []string{"tz"}},
		{"nick blank", mut(func(i *model.OnboardingInput) { i.Nickname = "   " }), []string{"nickname"}},
		{"nick empty", mut(func(i *model.OnboardingInput) { i.Nickname = "" }), []string{"nickname"}},
		{"nick 21", mut(func(i *model.OnboardingInput) { i.Nickname = strings.Repeat("a", 21) }), []string{"nickname"}},
		{"nick 20 han", mut(func(i *model.OnboardingInput) { i.Nickname = strings.Repeat("宠", 20) }), nil},
		{"nick 21 han", mut(func(i *model.OnboardingInput) { i.Nickname = strings.Repeat("宠", 21) }), []string{"nickname"}},
		{"nick 20 emoji", mut(func(i *model.OnboardingInput) { i.Nickname = strings.Repeat("🐶", 20) }), nil},
		{"nick control char", mut(func(i *model.OnboardingInput) { i.Nickname = "a\nb" }), []string{"nickname"}},
		{"nick bidi override", mut(func(i *model.OnboardingInput) { i.Nickname = "ab‮cd" }), []string{"nickname"}},
		{"nick zero width", mut(func(i *model.OnboardingInput) { i.Nickname = "a​b" }), []string{"nickname"}},
		{"nick hangul filler only", mut(func(i *model.OnboardingInput) { i.Nickname = "ㅤ" }), []string{"nickname"}},
		{"nick halfwidth hangul filler", mut(func(i *model.OnboardingInput) { i.Nickname = "abﾠ" }), []string{"nickname"}},
		{"nick braille blank", mut(func(i *model.OnboardingInput) { i.Nickname = "⠀" }), []string{"nickname"}},
		{"nick zwj between letters", mut(func(i *model.OnboardingInput) { i.Nickname = "Bo‍b" }), []string{"nickname"}},
		{"nick zwj trailing", mut(func(i *model.OnboardingInput) { i.Nickname = "\U0001F468‍" }), []string{"nickname"}},
		{"nick family emoji", mut(func(i *model.OnboardingInput) { i.Nickname = "\U0001F468‍\U0001F469‍\U0001F467" }), nil},
		{"nick rainbow flag", mut(func(i *model.OnboardingInput) { i.Nickname = "\U0001F3F3️‍\U0001F308" }), nil},
		{"nick skin tone technologist", mut(func(i *model.OnboardingInput) { i.Nickname = "\U0001F9D1\U0001F3FD‍\U0001F4BB" }), nil},
		{"nick korean", mut(func(i *model.OnboardingInput) { i.Nickname = "한글이름" }), nil},
		{"many", model.OnboardingInput{}, []string{"native_lang", "mbti", "nickname", "age_band", "tz"}},
		{"tz Tokyo works without OS zoneinfo", mut(func(i *model.OnboardingInput) { i.TZ = "Asia/Tokyo" }), nil},
	}
	for _, c := range cases {
		if got := ValidateOnboarding(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: Validate() = %v, want %v", c.name, got, c.want)
		}
	}
}

// --- integration ---

type clk struct{ t time.Time }

func (c *clk) now() time.Time { return c.t }

func setupUser(t *testing.T) (*UserService, *pgxpool.Pool, *clk) {
	t.Helper()
	pool := dbtest.Reset(t)
	c := &clk{t: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	svc := NewUserService(repository.NewUserRepo(pool), repository.NewRoomRepo(pool), repository.NewRoomMemberRepo(pool),
		repository.NewInviteCodeRepo(pool), repository.NewTxManager(pool), 7, c.now)
	return svc, pool, c
}

func newUser(t *testing.T, p *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := p.QueryRow(bg, `insert into users default values returning id::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestOnboard_CreatesProfileRoomSeatCode(t *testing.T) {
	svc, pool, c := setupUser(t)
	uid := newUser(t, pool)
	res, err := svc.Onboard(bg, uid, valid())
	if err != nil {
		t.Fatal(err)
	}
	if res.Pet.Type != "happy_shiba" || res.Pet.MBTI != "ENFP" || !res.User.Onboarded ||
		res.Room.MemberCount != 1 || res.Room.Capacity != 5 || !codeRE.MatchString(res.InviteCode.Code) {
		t.Fatalf("result = %+v", res)
	}

	var nick, lang, mbti, pet, age, tz string
	var onboarded bool
	err = pool.QueryRow(bg, `select nickname, native_lang, mbti, pet_type, age_band, tz, onboarded_at is not null
		from users where id=$1`, uid).Scan(&nick, &lang, &mbti, &pet, &age, &tz, &onboarded)
	if err != nil {
		t.Fatal(err)
	}
	if nick != "Mimi" || lang != "ja" || mbti != "ENFP" || pet != "happy_shiba" || age != "18_plus" || tz != "Asia/Tokyo" || !onboarded {
		t.Fatalf("users row: %q %q %q %q %q %q %v", nick, lang, mbti, pet, age, tz, onboarded)
	}
	var roomID string
	var members, capacity int
	if err := pool.QueryRow(bg, `select id::text, member_count, capacity from rooms where owner_id=$1`, uid).Scan(&roomID, &members, &capacity); err != nil {
		t.Fatal(err)
	}
	if members != 1 || capacity != 5 || roomID != res.Room.ID {
		t.Fatalf("room: %s %d %d", roomID, members, capacity)
	}
	if n := count(t, pool, `select count(*) from room_members where room_id=$1 and user_id=$2
		and seat_no=1 and role='owner' and left_at is null`, roomID, uid); n != 1 {
		t.Fatalf("owner seat rows = %d", n)
	}
	var src, status string
	var issuedTo *string
	var expires time.Time
	err = pool.QueryRow(bg, `select source, status, issued_to::text, expires_at from invite_codes where code=$1 and room_id=$2`,
		res.InviteCode.Code, roomID).Scan(&src, &status, &issuedTo, &expires)
	if err != nil {
		t.Fatal(err)
	}
	if src != "signup" || status != "unused" || issuedTo == nil || *issuedTo != uid || !expires.Equal(c.t.Add(7*24*time.Hour)) {
		t.Fatalf("code row: %s %s %v %v", src, status, issuedTo, expires)
	}
}

func TestOnboard_IdempotentIgnoresSecondBody(t *testing.T) {
	svc, pool, _ := setupUser(t)
	uid := newUser(t, pool)
	first, err := svc.Onboard(bg, uid, valid())
	if err != nil {
		t.Fatal(err)
	}
	other := model.OnboardingInput{NativeLang: "ko", MBTI: "INTJ", Nickname: "Other", AgeBand: "13_17", TZ: "Asia/Seoul"}
	second, err := svc.Onboard(bg, uid, other)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("first %+v != second %+v", first, second)
	}
	// An invalid body on an already-onboarded user is also ignored.
	if _, err := svc.Onboard(bg, uid, model.OnboardingInput{}); err != nil {
		t.Fatalf("invalid retry body should be ignored, got %v", err)
	}
	if count(t, pool, `select count(*) from rooms`) != 1 || count(t, pool, `select count(*) from invite_codes`) != 1 {
		t.Fatal("want exactly one room and one code")
	}
	var mbti string
	pool.QueryRow(bg, `select mbti from users where id=$1`, uid).Scan(&mbti)
	if mbti != "ENFP" {
		t.Fatalf("mbti changed to %q", mbti)
	}
}

func TestOnboard_ConcurrentCallsCreateOneRoom(t *testing.T) {
	svc, pool, _ := setupUser(t)
	uid := newUser(t, pool)
	const n = 8
	rooms := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := svc.Onboard(bg, uid, valid())
			rooms[i], errs[i] = r.Room.ID, err
		}(i)
	}
	wg.Wait()
	for i := range rooms {
		if errs[i] != nil || rooms[i] != rooms[0] {
			t.Fatalf("goroutine %d: room=%q err=%v", i, rooms[i], errs[i])
		}
	}
	if count(t, pool, `select count(*) from rooms`) != 1 {
		t.Fatal("want exactly one room")
	}
}

func TestOnboard_InvalidInputWritesNothing(t *testing.T) {
	svc, pool, _ := setupUser(t)
	uid := newUser(t, pool)
	in := valid()
	in.MBTI, in.TZ = "nope", "Local"
	_, err := svc.Onboard(bg, uid, in)
	var ve *ValidationError
	if !errors.As(err, &ve) || !reflect.DeepEqual(ve.Fields, []string{"mbti", "tz"}) {
		t.Fatalf("err = %v", err)
	}
	if count(t, pool, `select count(*) from rooms`) != 0 || count(t, pool, `select count(*) from invite_codes`) != 0 ||
		count(t, pool, `select count(*) from users where onboarded_at is not null`) != 0 {
		t.Fatal("invalid input must not write anything")
	}
}

func TestOnboard_CodeCollisionRetries(t *testing.T) {
	svc, pool, _ := setupUser(t)
	// Occupy a code owned by someone else's room.
	other := newUser(t, pool)
	var otherRoom string
	if err := pool.QueryRow(bg, `insert into rooms(owner_id, member_count) values($1,1) returning id::text`, other).Scan(&otherRoom); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(bg, `insert into invite_codes(code, room_id, source, expires_at) values('AAAAAAAA',$1,'zero',now()+interval '1 day')`, otherRoom); err != nil {
		t.Fatal(err)
	}
	calls := 0
	svc.newCode = func() (string, error) {
		calls++
		if calls == 1 {
			return "AAAAAAAA", nil
		}
		return "BBBBBBBB", nil
	}
	res, err := svc.Onboard(bg, newUser(t, pool), valid())
	if err != nil {
		t.Fatal(err)
	}
	if res.InviteCode.Code != "BBBBBBBB" || calls != 2 {
		t.Fatalf("code=%q calls=%d", res.InviteCode.Code, calls)
	}
}

func TestOnboard_UnknownUserAndDisabledUser(t *testing.T) {
	svc, pool, _ := setupUser(t)
	if _, err := svc.Onboard(bg, "00000000-0000-0000-0000-000000000000", valid()); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := svc.Onboard(bg, "not-a-uuid", valid()); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("malformed id: %v", err)
	}
	for _, status := range []string{"banned", "deleted"} {
		uid := newUser(t, pool)
		pool.Exec(bg, `update users set status=$1 where id=$2`, status, uid)
		if _, err := svc.Onboard(bg, uid, valid()); !errors.Is(err, ErrAccountDisabled) {
			t.Fatalf("%s: %v", status, err)
		}
	}
	if count(t, pool, `select count(*) from rooms`) != 0 || count(t, pool, `select count(*) from invite_codes`) != 0 {
		t.Fatal("nothing may be written")
	}
}

func TestMe_BeforeAndAfterOnboarding(t *testing.T) {
	svc, pool, _ := setupUser(t)
	uid := newUser(t, pool)
	p, err := svc.Me(bg, uid)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != uid || p.Onboarded || p.MBTI != nil || p.Nickname != nil || p.PetType != nil {
		t.Fatalf("before: %+v", p)
	}
	if _, err := svc.Onboard(bg, uid, valid()); err != nil {
		t.Fatal(err)
	}
	p, _ = svc.Me(bg, uid)
	if !p.Onboarded || p.MBTI == nil || *p.MBTI != "ENFP" || *p.PetType != "happy_shiba" || *p.Nickname != "Mimi" ||
		*p.NativeLang != "ja" || *p.AgeBand != "18_plus" || *p.TZ != "Asia/Tokyo" {
		t.Fatalf("after: %+v", p)
	}
	pool.Exec(bg, `update users set status='banned' where id=$1`, uid)
	if _, err := svc.Me(bg, uid); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("banned: %v", err)
	}
	if _, err := svc.Me(bg, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}
