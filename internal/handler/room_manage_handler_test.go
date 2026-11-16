package handler_test

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jinheng0301/petbridge/internal/service"
)

const noRoom = "00000000-0000-0000-0000-000000000000"

// joiner onboards a new 18_plus user and seats them in owner's room through the
// real join endpoint, using a fresh extra code.
func (a *app) joiner(t *testing.T, owner member, email string) member {
	t.Helper()
	m := a.onboarded(t, email, "18_plus")
	code, err := service.NewCode()
	if err != nil {
		t.Fatal(err)
	}
	a.extraCode(t, owner, code)
	if w := a.join(m, code); w.Code != 200 {
		t.Fatalf("join %s: %d %s", email, w.Code, w.Body.String())
	}
	return m
}

func (a *app) do(method, path string, m member) *httptest.ResponseRecorder {
	return call(a.r, method, "/api/v1"+path, "", m.token)
}

func (a *app) roomsOf(t *testing.T, m member) []any {
	t.Helper()
	list, _ := decode(t, a.do("GET", "/rooms/mine", m))["data"].([]any)
	return list
}

func assertErr(t *testing.T, w *httptest.ResponseRecorder, status int, code string) map[string]any {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d want %d: %s", w.Code, status, w.Body.String())
	}
	e := errOf(t, w)
	if e["code"] != code {
		t.Fatalf("code=%v want %s: %s", e["code"], code, w.Body.String())
	}
	return e
}

func TestRemove_HTTP(t *testing.T) {
	a := newApp(t, nil)
	owner := a.onboarded(t, "o@local", "18_plus")
	b := a.joiner(t, owner, "b@local")
	a.joiner(t, owner, "c@local")

	w := a.do("DELETE", "/rooms/"+owner.roomID+"/members/"+b.id, owner)
	if w.Code != 200 {
		t.Fatalf("remove: %d %s", w.Code, w.Body.String())
	}
	room := data(t, w)["room"].(map[string]any)
	if room["id"] != owner.roomID || room["member_count"] != float64(2) || room["status"] != "open" {
		t.Fatalf("room = %v", room)
	}
	if list := a.roomsOf(t, b); len(list) != 1 {
		t.Fatalf("removed user still sees %d rooms: %v", len(list), list)
	}

	fresh, _ := service.NewCode()
	a.extraCode(t, owner, fresh)
	assertErr(t, a.join(b, fresh), 404, "CODE_INVALID")
}

func TestLeave_HTTP(t *testing.T) {
	a := newApp(t, nil)
	owner := a.onboarded(t, "o@local", "18_plus")
	b := a.joiner(t, owner, "b@local")
	c := a.joiner(t, owner, "c@local")
	stranger := a.onboarded(t, "s@local", "18_plus")
	path := "/rooms/" + owner.roomID + "/leave"

	w := a.do("POST", path, c)
	if w.Code != 204 || w.Body.Len() != 0 {
		t.Fatalf("leave: %d %q", w.Code, w.Body.String())
	}
	if w := a.do("POST", path, c); w.Code != 204 {
		t.Fatalf("repeat leave: %d %s", w.Code, w.Body.String())
	}
	assertErr(t, a.do("POST", path, owner), 409, "OWNER_CANNOT_LEAVE")
	assertErr(t, a.do("POST", path, stranger), 404, "ROOM_NOT_FOUND")

	// A removed member cannot tell it from having left.
	if w := a.do("DELETE", "/rooms/"+owner.roomID+"/members/"+b.id, owner); w.Code != 200 {
		t.Fatalf("remove: %d %s", w.Code, w.Body.String())
	}
	kicked := a.do("POST", path, b)
	if kicked.Code != w.Code || kicked.Body.String() != w.Body.String() {
		t.Fatalf("removed user got %d %q, a member who left got %d %q",
			kicked.Code, kicked.Body.String(), w.Code, w.Body.String())
	}
}

func TestDissolve_HTTP(t *testing.T) {
	a := newApp(t, nil)
	owner := a.onboarded(t, "o@local", "18_plus")
	b := a.joiner(t, owner, "b@local")
	a.joiner(t, owner, "c@local")
	stranger := a.onboarded(t, "s@local", "18_plus")
	path := "/rooms/" + owner.roomID + "/dissolve"

	assertErr(t, a.do("POST", path, b), 403, "NOT_ROOM_OWNER")
	assertErr(t, a.do("POST", path, stranger), 404, "ROOM_NOT_FOUND")
	w := a.do("POST", path, owner)
	if w.Code != 200 {
		t.Fatalf("dissolve: %d %s", w.Code, w.Body.String())
	}
	room := data(t, w)["room"].(map[string]any)
	if room["member_count"] != float64(1) || room["status"] != "open" {
		t.Fatalf("room = %v", room)
	}
	if list := a.roomsOf(t, b); len(list) != 1 {
		t.Fatalf("dissolved member still sees %d rooms", len(list))
	}
}

func TestRegen_HTTP(t *testing.T) {
	a := newApp(t, nil)
	owner := a.onboarded(t, "o@local", "18_plus")
	path := "/rooms/" + owner.roomID + "/codes"

	w := a.do("POST", path, owner)
	if w.Code != 201 {
		t.Fatalf("regen: %d %s", w.Code, w.Body.String())
	}
	d := data(t, w)
	if len(d["code"].(string)) != 8 || d["expires_at"] == nil {
		t.Fatalf("data = %v", d)
	}

	reason := func(t *testing.T, owner member) any {
		t.Helper()
		e := assertErr(t, a.do("POST", "/rooms/"+owner.roomID+"/codes", owner), 409, "CODE_ISSUE_LIMIT")
		return e["details"].(map[string]any)["reason"]
	}
	seed := func(t *testing.T, m member, n int, source, status string) {
		t.Helper()
		for i := 0; i < n; i++ {
			code, _ := service.NewCode()
			a.exec(t, `insert into invite_codes(code, room_id, issued_to, source, status, expires_at)
				values($1,$2,$3,$4,$5, now() + interval '1 day')`, code, m.roomID, m.id, source, status)
		}
	}

	full := a.onboarded(t, "full@local", "18_plus")
	for i := 0; i < 4; i++ {
		a.joiner(t, full, fmt.Sprintf("f%d@local", i))
	}
	if got := reason(t, full); got != "room_full" {
		t.Fatalf("full room: reason=%v", got)
	}
	enough := a.onboarded(t, "enough@local", "18_plus")
	seed(t, enough, 3, "owner_regen", "unused")
	if got := reason(t, enough); got != "enough_unused" {
		t.Fatalf("reason=%v", got)
	}
	daily := a.onboarded(t, "daily@local", "18_plus")
	seed(t, daily, 3, "owner_regen", "void")
	if got := reason(t, daily); got != "daily_limit" {
		t.Fatalf("reason=%v", got)
	}
	lifetime := a.onboarded(t, "life@local", "18_plus")
	seed(t, lifetime, 20, "join_reward", "void")
	if got := reason(t, lifetime); got != "lifetime_limit" {
		t.Fatalf("reason=%v", got)
	}
}

func TestManage_ErrorMapping(t *testing.T) {
	a := newApp(t, nil)
	owner := a.onboarded(t, "o@local", "18_plus")
	b := a.joiner(t, owner, "b@local")
	stranger := a.onboarded(t, "s@local", "18_plus")
	room := "/rooms/" + owner.roomID

	cases := []struct {
		name         string
		method, path string
		caller       member
		status       int
		code         string
	}{
		{"member removes someone", "DELETE", room + "/members/" + owner.id, b, 403, "NOT_ROOM_OWNER"},
		{"member regenerates", "POST", room + "/codes", b, 403, "NOT_ROOM_OWNER"},
		{"stranger removes", "DELETE", room + "/members/" + b.id, stranger, 404, "ROOM_NOT_FOUND"},
		{"unknown room", "POST", "/rooms/" + noRoom + "/dissolve", owner, 404, "ROOM_NOT_FOUND"},
		{"owner leaves", "POST", room + "/leave", owner, 409, "OWNER_CANNOT_LEAVE"},
		{"owner removes self", "DELETE", room + "/members/" + owner.id, owner, 400, "VALIDATION_FAILED"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assertErr(t, a.do(c.method, c.path, c.caller), c.status, c.code)
		})
	}
}

func TestManage_BadIDs(t *testing.T) {
	a := newApp(t, nil)
	owner := a.onboarded(t, "o@local", "18_plus")
	long := strings.Repeat("a", 10*1024)

	for _, id := range []string{"abc", long} {
		for _, r := range [][2]string{
			{"POST", "/rooms/" + id + "/leave"}, {"POST", "/rooms/" + id + "/dissolve"},
			{"POST", "/rooms/" + id + "/codes"}, {"DELETE", "/rooms/" + id + "/members/" + owner.id},
		} {
			w := a.do(r[0], r[1], owner)
			if w.Code != 404 {
				t.Errorf("%s %.40s: %d, want 404", r[0], r[1], w.Code)
			}
		}
	}
	for _, target := range []string{"abc", long} {
		if w := a.do("DELETE", "/rooms/"+owner.roomID+"/members/"+target, owner); w.Code != 200 {
			t.Errorf("bad target %.10s: %d %s, want idempotent 200", target, w.Code, w.Body.String())
		}
	}
	for _, r := range [][2]string{
		{"POST", "/rooms/" + owner.roomID + "/leave"}, {"POST", "/rooms/" + owner.roomID + "/dissolve"},
		{"POST", "/rooms/" + owner.roomID + "/codes"}, {"DELETE", "/rooms/" + owner.roomID + "/members/" + owner.id},
	} {
		if w := a.do(r[0], r[1], member{}); w.Code != 401 {
			t.Errorf("%s %s without token: %d", r[0], r[1], w.Code)
		}
	}
}

func TestManage_NoCodeInResponsesOrLogs(t *testing.T) {
	a := newApp(t, nil)
	owner := a.onboarded(t, "o@local", "18_plus")
	b := a.joiner(t, owner, "b@local")
	room := "/rooms/" + owner.roomID

	var bodies []string
	for _, c := range []struct {
		method, path string
		caller       member
	}{
		{"DELETE", room + "/members/" + owner.id, owner}, {"POST", room + "/dissolve", b},
		{"POST", room + "/leave", owner}, {"POST", room + "/codes", b},
	} {
		w := a.do(c.method, c.path, c.caller)
		if w.Code < 400 {
			t.Fatalf("%s %s unexpectedly succeeded", c.method, c.path)
		}
		bodies = append(bodies, w.Body.String())
	}
	for _, s := range append(bodies, a.logs.String()) {
		if strings.Contains(s, owner.code) {
			t.Fatalf("full invite code found in %q", s)
		}
	}
}
