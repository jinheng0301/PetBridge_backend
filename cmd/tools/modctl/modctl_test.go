package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/platform/db/dbtest"
)

var bg = context.Background()

var fixedNow = func() time.Time { return time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC) }

func mkUser(t *testing.T, p *pgxpool.Pool, nick, age string) string {
	t.Helper()
	var id string
	if err := p.QueryRow(bg, `insert into users(nickname, age_band) values($1, $2) returning id::text`, nick, age).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func mkRoom(t *testing.T, p *pgxpool.Pool, owner string) string {
	t.Helper()
	var id string
	if err := p.QueryRow(bg, `insert into rooms(owner_id, member_count) values($1,1) returning id::text`, owner).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func run(t *testing.T, p *pgxpool.Pool, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := Run(bg, p, args, &buf, fixedNow)
	return buf.String(), err
}

func TestModctl_BanRevokesAndUnban(t *testing.T) {
	p := dbtest.Reset(t)
	u := mkUser(t, p, "ban-me", "18_plus")
	for _, h := range []string{"h1", "h2"} {
		if _, err := p.Exec(bg, `insert into refresh_tokens(user_id, token_hash, family_id, expires_at) values($1,$2, gen_random_uuid(), now()+interval '1 day')`, u, h); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := run(t, p, "ban", u); err != nil {
		t.Fatal(err)
	}
	var status string
	var live int
	p.QueryRow(bg, `select status from users where id=$1`, u).Scan(&status)
	p.QueryRow(bg, `select count(*) from refresh_tokens where user_id=$1 and revoked_at is null`, u).Scan(&live)
	if status != "banned" || live != 0 {
		t.Fatalf("status=%s live tokens=%d", status, live)
	}
	if _, err := run(t, p, "unban", u); err != nil {
		t.Fatal(err)
	}
	p.QueryRow(bg, `select status from users where id=$1`, u).Scan(&status)
	if status != "active" {
		t.Fatalf("status=%s", status)
	}
	// unban only works on banned accounts
	if _, err := run(t, p, "unban", u); err == nil {
		t.Fatal("unban of an active account must fail")
	}
	d := mkUser(t, p, "gone", "18_plus")
	p.Exec(bg, `update users set status='deleted' where id=$1`, d)
	if _, err := run(t, p, "ban", d); err == nil {
		t.Fatal("ban of a deleted account must fail")
	}
}

func TestModctl_FlagsListAndAck(t *testing.T) {
	p := dbtest.Reset(t)
	u := mkUser(t, p, "a", "18_plus")
	room := mkRoom(t, p, u)
	var mid string
	if err := p.QueryRow(bg, `insert into messages(id, room_id, sender_id, scope, kind, src_lang, body, client_msg_id)
		values(gen_random_uuid(), $1, $2, 'room', 'text', 'ja', 'BODY-FROM-MESSAGE', 'c1') returning id::text`, room, u).Scan(&mid); err != nil {
		t.Fatal(err)
	}
	var f1, f2 string
	if err := p.QueryRow(bg, `insert into mod_flags(stage, verdict, source, terms, user_id, text) values('outgoing','review','wordlist','{x}',$1,'TEXT-IN-FLAG') returning id::text`, u).Scan(&f1); err != nil {
		t.Fatal(err)
	}
	if err := p.QueryRow(bg, `insert into mod_flags(stage, verdict, source, user_id, room_id, message_id) values('outgoing','block','model',$1,$2,$3) returning id::text`, u, room, mid).Scan(&f2); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, p, "flags")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "TEXT-IN-FLAG") || !strings.Contains(out, "BODY-FROM-MESSAGE") {
		t.Fatalf("output:\n%s", out)
	}
	if _, err := run(t, p, "flags", "ack", f1); err != nil {
		t.Fatal(err)
	}
	out, _ = run(t, p, "flags")
	if strings.Contains(out, "TEXT-IN-FLAG") || !strings.Contains(out, "BODY-FROM-MESSAGE") {
		t.Fatalf("after ack:\n%s", out)
	}
	out, _ = run(t, p, "flags", "-status", "reviewed")
	if !strings.Contains(out, "TEXT-IN-FLAG") {
		t.Fatalf("reviewed list:\n%s", out)
	}
}

func TestModctl_FlagsShowDeliveryTextForTranslation(t *testing.T) {
	p := dbtest.Reset(t)
	u := mkUser(t, p, "a", "18_plus")
	r := mkUser(t, p, "b", "18_plus")
	room := mkRoom(t, p, u)
	var mid, did string
	if err := p.QueryRow(bg, `insert into messages(id, room_id, sender_id, scope, kind, src_lang, body, client_msg_id)
		values(gen_random_uuid(), $1, $2, 'room', 'text', 'ja', 'SOURCE-BODY', 'c1') returning id::text`, room, u).Scan(&mid); err != nil {
		t.Fatal(err)
	}
	if err := p.QueryRow(bg, `insert into deliveries(message_id, receiver_id, target_lang, base_text, translation_status)
		values($1,$2,'en','TRANSLATED-TEXT','done') returning id::text`, mid, r).Scan(&did); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(bg, `insert into mod_flags(stage, verdict, source, user_id, room_id, message_id, delivery_id)
		values('translation','block','model',$1,$2,$3,$4)`, u, room, mid, did); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, p, "flags")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "TRANSLATED-TEXT") || strings.Contains(out, "SOURCE-BODY") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestModctl_ReportsOrderAndResolve(t *testing.T) {
	p := dbtest.Reset(t)
	a := mkUser(t, p, "alice", "18_plus")
	b := mkUser(t, p, "bob", "18_plus")
	room := mkRoom(t, p, a)
	var older, minor string
	if err := p.QueryRow(bg, `insert into reports(reporter_id, target_id, room_id, reason, created_at) values($1,$2,$3,'spam', now()-interval '1 hour') returning id::text`, a, b, room).Scan(&older); err != nil {
		t.Fatal(err)
	}
	if err := p.QueryRow(bg, `insert into reports(reporter_id, target_id, room_id, reason, created_at) values($1,$2,$3,'minor_safety', now()-interval '2 hour') returning id::text`, b, a, room).Scan(&minor); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, p, "reports")
	if err != nil {
		t.Fatal(err)
	}
	if i, j := strings.Index(out, minor), strings.Index(out, older); i < 0 || j < 0 || i > j {
		t.Fatalf("minor_safety should come first:\n%s", out)
	}
	if _, err := run(t, p, "reports", "resolve", older, "-note", "done"); err != nil {
		t.Fatal(err)
	}
	var status string
	var note *string
	var at *time.Time
	p.QueryRow(bg, `select status, handled_note, handled_at from reports where id=$1`, older).Scan(&status, &note, &at)
	if status != "resolved" || note == nil || *note != "done" || at == nil {
		t.Fatalf("status=%s note=%v at=%v", status, note, at)
	}
	if _, err := run(t, p, "reports", "dismiss", minor); err != nil {
		t.Fatal(err)
	}
	p.QueryRow(bg, `select status from reports where id=$1`, minor).Scan(&status)
	if status != "dismissed" {
		t.Fatalf("status=%s", status)
	}
}

func TestModctl_UnknownCommand(t *testing.T) {
	p := dbtest.Reset(t)
	for _, args := range [][]string{{}, {"nope"}, {"ban"}, {"flags", "ack"}} {
		_, err := run(t, p, args...)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "usage") {
			t.Fatalf("args %v: err=%v", args, err)
		}
	}
}
