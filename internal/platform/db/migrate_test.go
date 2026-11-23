package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/platform/db"
	"github.com/jinheng0301/petbridge/internal/platform/db/dbtest"
)

var bg = context.Background()

func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	if err == nil {
		return ""
	}
	return "non-pg: " + err.Error()
}

func wantState(t *testing.T, err error, want string) {
	t.Helper()
	if got := sqlState(err); got != want {
		t.Fatalf("SQLSTATE = %q, want %q (err=%v)", got, want, err)
	}
}

func newUser(t *testing.T, p *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := p.QueryRow(bg, `insert into users default values returning id::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func newRoom(t *testing.T, p *pgxpool.Pool, owner string) string {
	t.Helper()
	var id string
	if err := p.QueryRow(bg, `insert into rooms(owner_id) values($1) returning id::text`, owner).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func addMember(p *pgxpool.Pool, room, user string, seat int) error {
	_, err := p.Exec(bg, `insert into room_members(room_id,user_id,seat_no,role) values($1,$2,$3,'member')`, room, user, seat)
	return err
}

func TestMigrateUp_Idempotent(t *testing.T) {
	dbtest.Reset(t)
	for i := 0; i < 2; i++ {
		if err := db.MigrateUp(bg, dbtest.URL()); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
}

func TestMigrateUp_CreatesAIUsageDaily(t *testing.T) {
	p := dbtest.Reset(t)
	var n int
	if err := p.QueryRow(bg, `select count(*) from information_schema.columns where table_name='ai_usage_daily'`).Scan(&n); err != nil || n != 7 {
		t.Fatalf("ai_usage_daily columns=%d err=%v", n, err)
	}
	u := newUser(t, p)
	_, err := p.Exec(bg, `insert into ai_usage_daily values('2026-11-22',$1,'other',1,0,0,0)`, u)
	wantState(t, err, "23514") // kind check
}

func TestMigrateUp_CreatesModerationTables(t *testing.T) {
	p := dbtest.Reset(t)
	for _, tbl := range []string{"mod_flags", "blocks", "reports"} {
		var n int
		if err := p.QueryRow(bg, `select count(*) from information_schema.tables where table_schema='public' and table_name=$1`, tbl).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s missing (n=%d err=%v)", tbl, n, err)
		}
	}
	var n int
	if err := p.QueryRow(bg, `select count(*) from information_schema.columns where table_name='deliveries' and column_name='mod_status'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("deliveries.mod_status missing (n=%d err=%v)", n, err)
	}
	u := newUser(t, p)
	_, err := p.Exec(bg, `insert into blocks(blocker_id, blocked_id) values($1,$1)`, u)
	wantState(t, err, "23514") // self-block check
}

func TestMigrateDownThenUp_RoundTrip(t *testing.T) {
	p := dbtest.Reset(t)
	for i := 0; i < 10; i++ { // roll back every migration, not just the latest
		st, err := db.MigrateStatus(bg, dbtest.URL())
		if err != nil {
			t.Fatal(err)
		}
		if st == "current version: 0" {
			break
		}
		if err := db.MigrateDown(bg, dbtest.URL()); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	err := p.QueryRow(bg, `select count(*) from information_schema.tables
		where table_schema='public' and table_name <> 'goose_db_version'`).Scan(&n)
	if err != nil || n != 0 {
		t.Fatalf("tables left after down: %d (err=%v)", n, err)
	}
	if err := db.MigrateUp(bg, dbtest.URL()); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateStatus_ReportsApplied(t *testing.T) {
	dbtest.Reset(t)
	s, err := db.MigrateStatus(bg, dbtest.URL())
	if err != nil || s == "" {
		t.Fatalf("status=%q err=%v", s, err)
	}
}

func TestRoomMembers_ActiveUniqueness(t *testing.T) {
	p := dbtest.Reset(t)
	room := newRoom(t, p, newUser(t, p))
	u := newUser(t, p)
	if err := addMember(p, room, u, 1); err != nil {
		t.Fatal(err)
	}
	wantState(t, addMember(p, room, u, 2), "23505")
	if _, err := p.Exec(bg, `update room_members set left_at=now(), left_reason='left' where room_id=$1 and user_id=$2`, room, u); err != nil {
		t.Fatal(err)
	}
	if err := addMember(p, room, u, 2); err != nil {
		t.Fatalf("rejoin after leaving should work: %v", err)
	}
}

func TestRoomMembers_SeatUniqueAmongActive(t *testing.T) {
	p := dbtest.Reset(t)
	room := newRoom(t, p, newUser(t, p))
	if err := addMember(p, room, newUser(t, p), 1); err != nil {
		t.Fatal(err)
	}
	wantState(t, addMember(p, room, newUser(t, p), 1), "23505")
	wantState(t, addMember(p, room, newUser(t, p), 6), "23514")
}

func TestRoomMembers_LeftReasonConsistency(t *testing.T) {
	p := dbtest.Reset(t)
	room := newRoom(t, p, newUser(t, p))
	_, err := p.Exec(bg, `insert into room_members(room_id,user_id,seat_no,role,left_at) values($1,$2,1,'member',now())`, room, newUser(t, p))
	wantState(t, err, "23514")
}

func TestRooms_MemberCapCheck(t *testing.T) {
	p := dbtest.Reset(t)
	room := newRoom(t, p, newUser(t, p))
	_, err := p.Exec(bg, `update rooms set member_count = 6 where id=$1`, room)
	wantState(t, err, "23514")
}

func TestRooms_OneRoomPerOwner(t *testing.T) {
	p := dbtest.Reset(t)
	owner := newUser(t, p)
	newRoom(t, p, owner)
	_, err := p.Exec(bg, `insert into rooms(owner_id) values($1)`, owner)
	wantState(t, err, "23505")
}

func TestInviteCodes_FormatCheck(t *testing.T) {
	p := dbtest.Reset(t)
	room := newRoom(t, p, newUser(t, p))
	ins := func(code string) error {
		_, err := p.Exec(bg, `insert into invite_codes(code,room_id,source,expires_at) values($1,$2,'zero',now()+interval '7 days')`, code, room)
		return err
	}
	for _, ok := range []string{"DEVA2345", "FULL2345"} {
		if err := ins(ok); err != nil {
			t.Errorf("%s rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"ABC0O1I2", "SHORT", "deva2345"} {
		if got := sqlState(ins(bad)); got != "23514" {
			t.Errorf("%s: SQLSTATE %q, want 23514", bad, got)
		}
	}
}

func TestUsers_ProfileColumnsNullableBeforeOnboarding(t *testing.T) {
	p := dbtest.Reset(t)
	newUser(t, p)
	_, err := p.Exec(bg, `insert into users(native_lang) values('fr')`)
	wantState(t, err, "23514")
}

func TestRefreshTokens_FamilyColumns(t *testing.T) {
	p := dbtest.Reset(t)
	u := newUser(t, p)
	_, err := p.Exec(bg, `insert into refresh_tokens(user_id, token_hash, expires_at) values($1,'h1',now()+interval '1 day')`, u)
	wantState(t, err, "23502") // family_id is not null
	_, err = p.Exec(bg, `insert into refresh_tokens(user_id, token_hash, expires_at, family_id)
		values($1,'h2',now()+interval '1 day', gen_random_uuid())`, u)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := p.QueryRow(bg, `select count(*) from information_schema.columns
		where table_name='refresh_tokens' and column_name='replaced_by'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("replaced_by column: n=%d err=%v", n, err)
	}
}

func TestMigration0003_AddsRewardForColumn(t *testing.T) {
	p := dbtest.Reset(t)
	var nullable string
	var refs int
	err := p.QueryRow(bg, `select is_nullable from information_schema.columns
		where table_name='invite_codes' and column_name='reward_for'`).Scan(&nullable)
	if err != nil || nullable != "YES" {
		t.Fatalf("nullable=%q err=%v", nullable, err)
	}
	err = p.QueryRow(bg, `select count(*) from pg_constraint k
		where k.conrelid='invite_codes'::regclass and k.confrelid='invite_codes'::regclass and k.contype='f'`).Scan(&refs)
	if err != nil || refs != 1 {
		t.Fatalf("self-referencing fk count=%d err=%v", refs, err)
	}
	// 0006 is the latest migration now: roll back 0006, 0005 and 0004 first, then 0003.
	for i := 0; i < 4; i++ {
		if err := db.MigrateDown(bg, dbtest.URL()); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := p.QueryRow(bg, `select count(*) from information_schema.columns
		where table_name='invite_codes' and column_name='reward_for'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("column after down: n=%d err=%v", n, err)
	}
}
