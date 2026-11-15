package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/platform/db/dbtest"
	"github.com/jinheng0301/petbridge/internal/service"
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

func TestRun_CreatesElevenUsersAndZeroCode(t *testing.T) {
	pool := dbtest.Reset(t)
	if err := Run(bg, pool, fixedNow); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, `select count(*) from users where onboarded_at is not null`); n != 11 {
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
	if err := Run(bg, pool, fixedNow); err != nil {
		t.Fatal(err)
	}
	tables := []string{
		`select count(*) from users`, `select count(*) from rooms`,
		`select count(*) from room_members`, `select count(*) from invite_codes`,
		`select count(*) from invite_codes where source='signup'`,
		`select count(*) from invite_codes where source='zero'`,
		`select count(*) from invite_codes where status='void'`,
		`select count(*) from invite_codes where source='join_reward'`,
	}
	before := make([]int, len(tables))
	for i, q := range tables {
		before[i] = count(t, pool, q)
	}
	if err := Run(bg, pool, fixedNow); err != nil {
		t.Fatalf("second run: %v", err)
	}
	for i, q := range tables {
		if n := count(t, pool, q); n != before[i] {
			t.Errorf("second run changed %q: %d -> %d", q, before[i], n)
		}
	}
	if before[0] != 11 || before[5] != 6 {
		t.Fatalf("users=%d zero codes=%d, want 11 and 6", before[0], before[5])
	}
}

func userID(t *testing.T, p *pgxpool.Pool, email string) string {
	t.Helper()
	var id string
	if err := p.QueryRow(bg, `select user_id::text from auth_identities where provider='dev' and subject=$1`, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRun_BuildsFullRoom(t *testing.T) {
	pool := dbtest.Reset(t)
	if err := Run(bg, pool, fixedNow); err != nil {
		t.Fatal(err)
	}
	full := userID(t, pool, "dev-full@local")
	var roomID, status string
	var members int
	if err := pool.QueryRow(bg, `select id::text, status, member_count from rooms where owner_id=$1`, full).Scan(&roomID, &status, &members); err != nil {
		t.Fatal(err)
	}
	if status != "full" || members != 5 {
		t.Fatalf("room status=%s member_count=%d", status, members)
	}
	if n := count(t, pool, `select count(*) from room_members where room_id=$1 and left_at is null`, roomID); n != 5 {
		t.Fatalf("active members = %d", n)
	}
	var st string
	if err := pool.QueryRow(bg, `select status from invite_codes where code='FULL2345'`).Scan(&st); err != nil || st != "void" {
		t.Fatalf("FULL2345 status=%q err=%v", st, err)
	}
	for _, c := range []string{"FULA2345", "FULB2345", "FULC2345", "FULD2345"} {
		if err := pool.QueryRow(bg, `select status from invite_codes where code=$1`, c).Scan(&st); err != nil || st != "used" {
			t.Fatalf("%s status=%q err=%v", c, st, err)
		}
	}
	for _, e := range []string{"a", "b", "c", "d", "e", "f"} {
		id := userID(t, pool, "dev-"+e+"@local")
		if n := count(t, pool, `select count(*) from room_members where user_id=$1 and left_at is null`, id); n != 1 {
			t.Errorf("dev-%s active memberships = %d", e, n)
		}
	}
}

func TestRun_FullCodeIsInvalidForJoin(t *testing.T) {
	pool := dbtest.Reset(t)
	if err := Run(bg, pool, fixedNow); err != nil {
		t.Fatal(err)
	}
	_, err := newJoinService(pool, fixedNow).Join(bg, userID(t, pool, "dev-b@local"), "127.0.0.1", "FULL2345")
	if !errors.Is(err, service.ErrCodeInvalid) {
		t.Fatalf("err = %v, want ErrCodeInvalid", err)
	}
}

func TestRun_DevaCodeStillJoinable(t *testing.T) {
	pool := dbtest.Reset(t)
	if err := Run(bg, pool, fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := newJoinService(pool, fixedNow).Join(bg, userID(t, pool, "dev-b@local"), "127.0.0.1", "DEVA2345"); err != nil {
		t.Fatalf("join DEVA2345: %v", err)
	}
}
