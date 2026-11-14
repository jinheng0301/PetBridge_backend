package main

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/platform/db/dbtest"
)

var bg = context.Background()

func count(t *testing.T, p *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := p.QueryRow(bg, q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

var fixedNow = func() time.Time { return time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) }

func TestRun_CreatesSixUsersAndZeroCode(t *testing.T) {
	pool := dbtest.Reset(t)
	if err := Run(bg, pool, fixedNow); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, `select count(*) from users where onboarded_at is not null`); n != 6 {
		t.Fatalf("onboarded users = %d", n)
	}
	want := map[string][3]string{ // email -> lang, mbti, age band
		"dev-a@local": {"ja", "ENFP", "18_plus"},
		"dev-b@local": {"ko", "INFP", "18_plus"},
		"dev-c@local": {"zh-Hant", "INTJ", "18_plus"},
		"dev-d@local": {"zh-Hans", "ESTJ", "18_plus"},
		"dev-e@local": {"en", "ISFP", "18_plus"},
		"dev-f@local": {"en", "ESFP", "13_17"},
	}
	for email, w := range want {
		var lang, mbti, age string
		err := pool.QueryRow(bg, `select u.native_lang, u.mbti, u.age_band from users u
			join auth_identities i on i.user_id=u.id where i.provider='dev' and i.subject=$1`, email).Scan(&lang, &mbti, &age)
		if err != nil {
			t.Fatalf("%s: %v", email, err)
		}
		if [3]string{lang, mbti, age} != w {
			t.Errorf("%s: got %s/%s/%s want %v", email, lang, mbti, age, w)
		}
	}

	var src, status string
	var issuedTo *string
	var roomOwner string
	var expires time.Time
	err := pool.QueryRow(bg, `select c.source, c.status, c.issued_to::text, r.owner_id::text, c.expires_at
		from invite_codes c join rooms r on r.id=c.room_id where c.code='DEVA2345'`).Scan(&src, &status, &issuedTo, &roomOwner, &expires)
	if err != nil {
		t.Fatal(err)
	}
	var aID string
	pool.QueryRow(bg, `select user_id::text from auth_identities where subject='dev-a@local'`).Scan(&aID)
	if src != "zero" || status != "unused" || issuedTo != nil || roomOwner != aID {
		t.Fatalf("zero code: %s %s %v owner=%s (A=%s)", src, status, issuedTo, roomOwner, aID)
	}
	if d := expires.Sub(fixedNow()); d < 9*365*24*time.Hour || d > 11*365*24*time.Hour {
		t.Fatalf("zero code lifetime = %v, want about 10 years", d)
	}
	if n := count(t, pool, `select count(*) from refresh_tokens`); n != 0 {
		t.Fatalf("seed must not leave refresh tokens (%d)", n)
	}
}

func TestRun_Idempotent(t *testing.T) {
	pool := dbtest.Reset(t)
	for i := 0; i < 2; i++ {
		if err := Run(bg, pool, fixedNow); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if count(t, pool, `select count(*) from users`) != 6 || count(t, pool, `select count(*) from rooms`) != 6 ||
		count(t, pool, `select count(*) from invite_codes where source='signup'`) != 6 ||
		count(t, pool, `select count(*) from invite_codes where source='zero'`) != 1 {
		t.Fatal("second run changed row counts")
	}
}
