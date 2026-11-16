package service

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// regenSvc returns a copy of the manage service with different limits.
func (e *joinEnv) regenSvc(cfg ManageConfig) *ManageService {
	s := *e.mgr
	s.cfg = cfg
	return &s
}

func defaultManageConfig() ManageConfig {
	return ManageConfig{CodeTTLDays: 7, RegenDailyLimit: 3, LifetimeCap: 20}
}

// owner makes a fresh onboarded 18_plus owner (new room, one signup code).
func (e *joinEnv) owner(t *testing.T) (ownerID, roomID string) {
	t.Helper()
	e.seq++
	id := e.user(t, fmt.Sprintf("owner-%d@x.test", e.seq), "18_plus")
	return id, e.roomOf(t, id)
}

// seedRegen stores n owner_regen codes issued at the given time.
func (e *joinEnv) seedRegen(t *testing.T, ownerID, roomID string, at time.Time, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		for {
			code, err := NewCode()
			if err != nil {
				t.Fatal(err)
			}
			ok, err := e.mgr.codes.InsertRegen(bg, code, roomID, ownerID, at, e.now.Add(24*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				break
			}
		}
	}
}

func wantReason(t *testing.T, err error, reason string) {
	t.Helper()
	var lim *CodeIssueLimitError
	if !errors.As(err, &lim) || lim.Reason != reason {
		t.Fatalf("err=%v want CodeIssueLimitError{%s}", err, reason)
	}
}

func TestRegen_Success(t *testing.T) {
	e := newJoinEnv(t)
	owner, room := e.owner(t)

	got, err := e.mgr.RegenCode(bg, owner, room)
	if err != nil {
		t.Fatal(err)
	}
	var source, issuedTo, roomID string
	var issued, expires time.Time
	if err := e.pool.QueryRow(bg, `select source, issued_to::text, room_id::text, issued_at, expires_at from invite_codes where code=$1`, got.Code).
		Scan(&source, &issuedTo, &roomID, &issued, &expires); err != nil {
		t.Fatal(err)
	}
	if source != "owner_regen" || issuedTo != owner || roomID != room {
		t.Fatalf("row source=%s issued_to=%s room=%s", source, issuedTo, roomID)
	}
	want := e.now.Add(7 * 24 * time.Hour)
	if !expires.Equal(want) || !got.ExpiresAt.Equal(want) || !issued.Equal(e.now) {
		t.Fatalf("expires db=%v returned=%v issued=%v, want %v / %v", expires, got.ExpiresAt, issued, want, e.now)
	}
	if st := e.codeStatus(t, got.Code); st != "unused" {
		t.Fatalf("status=%s", st)
	}
}

func TestRegen_Reasons(t *testing.T) {
	e := newJoinEnv(t)
	defaultCfg := defaultManageConfig()
	cases := []struct {
		name   string
		cfg    ManageConfig
		setup  func(t *testing.T, owner, room string)
		reason string
	}{
		{"room full", defaultCfg, func(t *testing.T, owner, room string) { e.fill(t, owner, 4) }, ReasonRoomFull},
		{"enough unused", defaultCfg, func(t *testing.T, owner, room string) {
			for i := 0; i < 3; i++ { // 1 signup + 3 = 4 unused = 4 free seats
				e.extraCode(t, owner)
			}
		}, ReasonEnoughUnused},
		{"daily limit", ManageConfig{CodeTTLDays: 7, RegenDailyLimit: 2, LifetimeCap: 20}, func(t *testing.T, owner, room string) {
			e.seedRegen(t, owner, room, e.now, 2)
		}, ReasonDailyLimit},
		{"lifetime limit", ManageConfig{CodeTTLDays: 7, RegenDailyLimit: 3, LifetimeCap: 1}, func(t *testing.T, owner, room string) {}, ReasonLifetimeLimit},
		{"full wins over everything", ManageConfig{CodeTTLDays: 7, RegenDailyLimit: 1, LifetimeCap: 1}, func(t *testing.T, owner, room string) {
			e.fill(t, owner, 4)
			e.seedRegen(t, owner, room, e.now, 2)
		}, ReasonRoomFull},
		{"enough unused wins over daily", ManageConfig{CodeTTLDays: 7, RegenDailyLimit: 1, LifetimeCap: 20}, func(t *testing.T, owner, room string) {
			e.seedRegen(t, owner, room, e.now, 3) // 1 signup + 3 regen = 4 unused, and daily limit 1 is also exceeded
		}, ReasonEnoughUnused},
		{"daily wins over lifetime", ManageConfig{CodeTTLDays: 7, RegenDailyLimit: 1, LifetimeCap: 2}, func(t *testing.T, owner, room string) {
			e.seedRegen(t, owner, room, e.now, 1) // 1 signup + 1 regen: both limits reached
		}, ReasonDailyLimit},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			owner, room := e.owner(t)
			c.setup(t, owner, room)
			before := count(t, e.pool, `select count(*) from invite_codes where room_id=$1`, room)
			_, err := e.regenSvc(c.cfg).RegenCode(bg, owner, room)
			wantReason(t, err, c.reason)
			if n := count(t, e.pool, `select count(*) from invite_codes where room_id=$1`, room); n != before {
				t.Fatalf("a refused regen changed the code count from %d to %d", before, n)
			}
		})
	}
}

func TestRegen_DayBoundary(t *testing.T) {
	e := newJoinEnv(t) // now = 2026-11-15T12:00Z
	cfg := ManageConfig{CodeTTLDays: 7, RegenDailyLimit: 1, LifetimeCap: 20}
	at := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	cases := []struct {
		name    string
		tz      any // nil stores NULL
		issued  time.Time
		blocked bool
	}{
		{"seoul: just before local midnight does not count", "Asia/Seoul", at("2026-11-14T14:59:00Z"), false},
		{"seoul: local midnight counts", "Asia/Seoul", at("2026-11-14T15:00:00Z"), true},
		{"null tz is UTC: before midnight", nil, at("2026-11-14T23:59:00Z"), false},
		{"null tz is UTC: at midnight", nil, at("2026-11-15T00:00:00Z"), true},
		{"invalid tz is UTC", "Not/AZone", at("2026-11-15T00:00:00Z"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			owner, room := e.owner(t)
			e.exec(t, `update users set tz=$2 where id=$1`, owner, c.tz)
			e.seedRegen(t, owner, room, c.issued, 1)
			_, err := e.regenSvc(cfg).RegenCode(bg, owner, room)
			if c.blocked {
				wantReason(t, err, ReasonDailyLimit)
			} else if err != nil {
				t.Fatalf("err=%v, want success", err)
			}
		})
	}
}

func TestRegen_ExpiredCodesDontCount(t *testing.T) {
	e := newJoinEnv(t)
	owner, room := e.owner(t)
	var extras []string
	for i := 0; i < 3; i++ {
		extras = append(extras, e.extraCode(t, owner))
	}
	if _, err := e.mgr.RegenCode(bg, owner, room); err == nil {
		t.Fatal("setup: 4 valid unused codes in 4 free seats must block regen")
	}
	for _, c := range extras {
		e.exec(t, `update invite_codes set expires_at=$2 where code=$1`, c, e.now.Add(-time.Hour))
	}
	if _, err := e.mgr.RegenCode(bg, owner, room); err != nil {
		t.Fatalf("expired codes must free their slots: %v", err)
	}
}

func TestRegen_AfterDissolve(t *testing.T) {
	e := newJoinEnv(t)
	owner, room := e.owner(t)
	e.fill(t, owner, 2)
	if _, err := e.mgr.Dissolve(bg, owner, room); err != nil {
		t.Fatal(err)
	}
	if _, err := e.mgr.RegenCode(bg, owner, room); err != nil {
		t.Fatalf("regen after dissolve: %v", err)
	}
}

func TestRegen_Authorization(t *testing.T) {
	e := newJoinEnv(t)
	owner, room := e.owner(t)
	m := e.fill(t, owner, 1)
	stranger := e.user(t, "s@x.test", "18_plus")
	for _, c := range []struct {
		name         string
		caller, room string
		want         error
	}{
		{"member is not owner", m[0], room, ErrNotRoomOwner},
		{"stranger", stranger, room, ErrRoomNotFound},
		{"malformed room", owner, "abc", ErrRoomNotFound},
		{"unknown room", owner, "00000000-0000-0000-0000-000000000000", ErrRoomNotFound},
	} {
		if _, err := e.mgr.RegenCode(bg, c.caller, c.room); !errors.Is(err, c.want) {
			t.Errorf("%s: err=%v want %v", c.name, err, c.want)
		}
	}
}

func TestRegen_CodeCollisionRetries(t *testing.T) {
	e := newJoinEnv(t)
	owner, room := e.owner(t)
	taken := e.signupCode(t, owner)

	svc := e.regenSvc(defaultManageConfig())
	calls := 0
	svc.newCode = func() (string, error) {
		calls++
		if calls == 1 {
			return taken, nil
		}
		return NewCode()
	}
	got, err := svc.RegenCode(bg, owner, room)
	if err != nil || got.Code == taken {
		t.Fatalf("code=%q err=%v", got.Code, err)
	}
	if n := count(t, e.pool, `select count(*) from invite_codes where issued_to=$1 and source='owner_regen'`, owner); n != 1 {
		t.Fatalf("owner_regen rows=%d, want 1", n)
	}

	always := e.regenSvc(defaultManageConfig())
	always.newCode = func() (string, error) { return taken, nil }
	_, err = always.RegenCode(bg, owner, room)
	var lim *CodeIssueLimitError
	if err == nil || errors.As(err, &lim) {
		t.Fatalf("err=%v, want a plain allocation error", err)
	}
}
