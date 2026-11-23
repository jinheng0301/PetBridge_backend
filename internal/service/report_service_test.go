package service

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jinheng0301/petbridge/internal/repository"
)

type reportEnv struct {
	*joinEnv
	svc      *ReportService
	logs     *bytes.Buffer
	room     string
	reporter string
	target   string
}

func newReportEnv(t *testing.T) *reportEnv {
	t.Helper()
	je := newJoinEnv(t)
	e := &reportEnv{joinEnv: je, logs: &bytes.Buffer{}}
	e.svc = e.serviceAt(je.now)
	e.reporter = je.user(t, "rep@example.com", "18_plus")
	e.target = je.user(t, "tgt@example.com", "18_plus")
	e.room = je.roomOf(t, e.reporter)
	e.seat(t, e.target, 2)
	return e
}

func (e *reportEnv) serviceAt(now time.Time) *ReportService {
	return NewReportService(repository.NewUserRepo(e.pool), repository.NewRoomMemberRepo(e.pool),
		repository.NewReportRepo(e.pool), func() time.Time { return now },
		slog.New(slog.NewTextHandler(e.logs, nil)))
}

func (e *reportEnv) seat(t *testing.T, userID string, seatNo int) {
	t.Helper()
	e.exec(t, `insert into room_members(room_id, user_id, seat_no, role) values($1,$2,$3,'member')`, e.room, userID, seatNo)
}

// message stores a room message from sender; withDelivery gives receiver a delivery of it.
func (e *reportEnv) message(t *testing.T, sender, receiver string, withDelivery bool) string {
	t.Helper()
	e.seq++
	var id string
	err := e.pool.QueryRow(bg, `insert into messages(id, room_id, sender_id, scope, kind, src_lang, body, client_msg_id)
		values(gen_random_uuid(), $1, $2, 'room', 'text', 'ja', 'hi', $3) returning id::text`,
		e.room, sender, fmt.Sprintf("c%d", e.seq)).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	if withDelivery {
		e.exec(t, `insert into deliveries(message_id, receiver_id, target_lang, translation_status) values($1,$2,'zh-Hans','done')`, id, receiver)
	}
	return id
}

func (e *reportEnv) in() ReportInput {
	return ReportInput{TargetUserID: e.target, RoomID: e.room, Reason: "spam"}
}

func TestReport_Validation(t *testing.T) {
	e := newReportEnv(t)
	bare := e.bareUser(t, "bare@example.com")
	mut := func(f func(*ReportInput)) ReportInput { in := e.in(); f(&in); return in }
	cases := []struct {
		name string
		in   ReportInput
		want []string
	}{
		{"target garbage", mut(func(i *ReportInput) { i.TargetUserID = "nope" }), []string{"target_user_id"}},
		{"target empty", mut(func(i *ReportInput) { i.TargetUserID = "" }), []string{"target_user_id"}},
		{"target self", mut(func(i *ReportInput) { i.TargetUserID = e.reporter }), []string{"target_user_id"}},
		{"target self upper", mut(func(i *ReportInput) { i.TargetUserID = strings.ToUpper(e.reporter) }), []string{"target_user_id"}},
		{"room garbage", mut(func(i *ReportInput) { i.RoomID = "x" }), []string{"room_id"}},
		{"reason unknown", mut(func(i *ReportInput) { i.Reason = "rude" }), []string{"reason"}},
		{"reason empty", mut(func(i *ReportInput) { i.Reason = "" }), []string{"reason"}},
		{"note too long", mut(func(i *ReportInput) { i.Note = strings.Repeat("字", 501) }), []string{"note"}},
		{"note long but padded", mut(func(i *ReportInput) { i.Note = "  " + strings.Repeat("a", 501) + "  " }), []string{"note"}},
		{"message garbage", mut(func(i *ReportInput) { i.MessageID = "m" }), []string{"message_id"}},
		{"all bad", ReportInput{TargetUserID: "x", RoomID: "y", Reason: "z", Note: strings.Repeat("a", 501), MessageID: "w"},
			[]string{"target_user_id", "room_id", "reason", "note", "message_id"}},
	}
	for _, c := range cases {
		_, err := e.svc.Report(bg, e.reporter, c.in)
		var ve *ValidationError
		if !errors.As(err, &ve) || strings.Join(ve.Fields, ",") != strings.Join(c.want, ",") {
			t.Fatalf("%s: err=%v want %v", c.name, err, c.want)
		}
	}
	if n := count(t, e.pool, `select count(*) from reports`); n != 0 {
		t.Fatalf("reports = %d", n)
	}

	// 500 characters, padded with whitespace, is fine and stored trimmed; blank becomes NULL.
	in := e.in()
	in.Note = "  " + strings.Repeat("字", 500) + " \n"
	if _, err := e.svc.Report(bg, e.reporter, in); err != nil {
		t.Fatal(err)
	}
	var note *string
	e.pool.QueryRow(bg, `select note from reports`).Scan(&note)
	if note == nil || len([]rune(*note)) != 500 {
		t.Fatalf("note %v", note)
	}
	e.exec(t, `update reports set status='dismissed'`)
	in.Note = "   "
	if _, err := e.svc.Report(bg, e.reporter, in); err != nil {
		t.Fatal(err)
	}
	if n := count(t, e.pool, `select count(*) from reports where note is null`); n != 1 {
		t.Fatalf("null notes = %d", n)
	}

	// A caller who has not onboarded is refused first.
	if _, err := e.svc.Report(bg, bare, ReportInput{}); !errors.Is(err, ErrOnboardingRequired) {
		t.Fatalf("bare err=%v", err)
	}
}

func TestReport_MustShareRoomEver(t *testing.T) {
	e := newReportEnv(t)
	// The target leaving does not stop a report.
	e.exec(t, `update room_members set left_at=now(), left_reason='left' where user_id=$1`, e.target)
	if _, err := e.svc.Report(bg, e.reporter, e.in()); err != nil {
		t.Fatalf("left target: %v", err)
	}
	// A stranger who never sat in the room cannot be reported there.
	stranger := e.user(t, "s@example.com", "18_plus")
	in := e.in()
	in.TargetUserID = stranger
	_, err := e.svc.Report(bg, e.reporter, in)
	wantFields(t, err, "room_id")
	// Nor can a reporter who never sat there.
	in = e.in()
	in.RoomID = e.roomOf(t, stranger)
	_, err = e.svc.Report(bg, e.reporter, in)
	wantFields(t, err, "room_id")
}

func TestReport_MessageChecks(t *testing.T) {
	e := newReportEnv(t)
	third := e.user(t, "third@example.com", "18_plus")
	e.seat(t, third, 3)

	good := e.message(t, e.target, e.reporter, true)
	byOther := e.message(t, third, e.reporter, true)
	noDelivery := e.message(t, e.target, e.reporter, false)
	for name, id := range map[string]string{"other sender": byOther, "no delivery": noDelivery,
		"unknown": "0190f0f0-0000-7000-8000-000000000001"} {
		in := e.in()
		in.MessageID = id
		_, err := e.svc.Report(bg, e.reporter, in)
		if err == nil {
			t.Fatalf("%s accepted", name)
		}
		wantFields(t, err, "message_id")
	}
	in := e.in()
	in.MessageID = strings.ToUpper(good)
	res, err := e.svc.Report(bg, e.reporter, in)
	if err != nil || res.ID == "" {
		t.Fatalf("good: %+v %v", res, err)
	}
	var mid string
	e.pool.QueryRow(bg, `select message_id::text from reports where id=$1`, res.ID).Scan(&mid)
	if mid != good {
		t.Fatalf("message_id = %s", mid)
	}
}

func TestReport_DedupesOpen(t *testing.T) {
	e := newReportEnv(t)
	a, err := e.svc.Report(bg, e.reporter, e.in())
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.svc.Report(bg, e.reporter, e.in())
	if err != nil || b.ID != a.ID || !b.CreatedAt.Equal(a.CreatedAt) {
		t.Fatalf("repeat %+v vs %+v err=%v", b, a, err)
	}
	// A report about a message is a different report.
	in := e.in()
	in.MessageID = e.message(t, e.target, e.reporter, true)
	c, err := e.svc.Report(bg, e.reporter, in)
	if err != nil || c.ID == a.ID {
		t.Fatalf("message report %+v err=%v", c, err)
	}
	if n := count(t, e.pool, `select count(*) from reports`); n != 2 {
		t.Fatalf("reports = %d", n)
	}
}

func TestReport_DailyLimit(t *testing.T) {
	e := newReportEnv(t)
	now := time.Date(2026, 11, 15, 22, 30, 0, 0, time.UTC)
	svc := e.serviceAt(now)
	first, err := svc.Report(bg, e.reporter, e.in())
	if err != nil {
		t.Fatal(err)
	}
	// Reports from before today's UTC midnight do not count.
	e.exec(t, `insert into reports(reporter_id, target_id, room_id, reason, status, created_at)
		select $1, $2, $3, 'spam', 'dismissed', $4 from generate_series(1,5)`,
		e.reporter, e.target, e.room, now.Add(-23*time.Hour))
	for i := 1; i < 20; i++ {
		in := e.in()
		in.MessageID = e.message(t, e.target, e.reporter, true)
		if _, err := svc.Report(bg, e.reporter, in); err != nil {
			t.Fatalf("report %d: %v", i+1, err)
		}
	}
	in := e.in()
	in.MessageID = e.message(t, e.target, e.reporter, true)
	_, err = svc.Report(bg, e.reporter, in)
	var rl *RateLimitedError
	if !errors.As(err, &rl) || rl.RetryAfter != 90*time.Minute {
		t.Fatalf("21st err=%v", err)
	}
	// Repeating one that is still open is not a new report.
	if r, err := svc.Report(bg, e.reporter, e.in()); err != nil || r.ID != first.ID {
		t.Fatalf("dup at limit: %+v %v", r, err)
	}
	// The next UTC day starts a fresh count (rows are stamped with the fake clock).
	e.exec(t, `update reports set created_at=$1 where created_at > $1`, now)
	next := e.serviceAt(now.Add(2 * time.Hour))
	if _, err := next.Report(bg, e.reporter, in); err != nil {
		t.Fatalf("next day: %v", err)
	}
}

func TestReport_PriorityLog(t *testing.T) {
	e := newReportEnv(t)
	minor := e.user(t, "minor@example.com", "13_17")
	e.seat(t, minor, 3)

	if _, err := e.svc.Report(bg, e.reporter, e.in()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.logs.String(), "priority") {
		t.Fatalf("plain report logged priority: %s", e.logs)
	}
	e.exec(t, `update reports set status='dismissed'`)
	in := e.in()
	in.Reason = "minor_safety"
	in.Note = "secret-note-text"
	if _, err := e.svc.Report(bg, e.reporter, in); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.logs.String(), "report needs priority review") {
		t.Fatalf("minor_safety not logged: %s", e.logs)
	}
	e.logs.Reset()
	e.exec(t, `update reports set status='dismissed'`)
	in = e.in()
	in.TargetUserID = minor
	in.Note = "secret-note-text"
	if _, err := e.svc.Report(bg, e.reporter, in); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.logs.String(), "report needs priority review") {
		t.Fatalf("13_17 target not logged: %s", e.logs)
	}
	if strings.Contains(e.logs.String(), "secret-note-text") {
		t.Fatalf("note leaked into logs: %s", e.logs)
	}
}
