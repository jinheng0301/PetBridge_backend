package handler_test

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jinheng0301/petbridge/internal/platform/ratelimit"
)

const joinPath = "/api/v1/rooms/join"

// errOf returns the error object of a failure response.
func errOf(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	e, _ := decode(t, w)["error"].(map[string]any)
	if e == nil {
		t.Fatalf("no error in %d %s", w.Code, w.Body.String())
	}
	return e
}

// displayForm formats a code the way users type it: lower case, XXXX-XXXX.
func displayForm(code string) string {
	return strings.ToLower(code[:4]) + "-" + strings.ToLower(code[4:])
}

func TestJoin_HTTPSuccess(t *testing.T) {
	a := newApp(t, nil)
	owner := a.onboarded(t, "a@local", "18_plus")
	b := a.onboarded(t, "b@local", "18_plus")

	w := a.join(b, displayForm(owner.code))
	if w.Code != 200 {
		t.Fatalf("join: %d %s", w.Code, w.Body.String())
	}
	d := data(t, w)
	room := d["room"].(map[string]any)
	if room["id"] != owner.roomID || room["member_count"] != float64(2) || room["status"] != "open" ||
		room["capacity"] == nil {
		t.Fatalf("room = %v", room)
	}
	if d["seat_no"] != float64(2) {
		t.Fatalf("seat_no = %v", d["seat_no"])
	}
	members, _ := d["members"].([]any)
	if len(members) != 2 {
		t.Fatalf("members = %v", d["members"])
	}
	for _, m := range members {
		mm := m.(map[string]any)
		for _, k := range []string{"user_id", "nickname", "pet_type", "seat_no", "role"} {
			if _, ok := mm[k]; !ok {
				t.Fatalf("member lacks %s: %v", k, mm)
			}
		}
	}
	rewards, ok := d["rewards"].([]any)
	if !ok || len(rewards) != 3 {
		t.Fatalf("rewards = %v", d["rewards"])
	}
	r0 := rewards[0].(map[string]any)
	if len(r0["code"].(string)) != 8 || r0["expires_at"] == nil {
		t.Fatalf("reward = %v", r0)
	}
	if strings.Contains(a.logs.String(), owner.code) {
		t.Fatal("full invite code found in logs")
	}
}

func TestJoin_RewardsEmptyArrayNotNull(t *testing.T) {
	a := newApp(t, nil)
	owner := a.onboarded(t, "a@local", "18_plus")
	b := a.onboarded(t, "b@local", "18_plus")
	// B's own room has no free slot for a reward: the grant is 0.
	a.exec(t, `update rooms set member_count = capacity - 1 where id=$1`, b.roomID)
	w := a.join(b, owner.code)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"rewards":[]`) {
		t.Fatalf("join: %d %s", w.Code, w.Body.String())
	}
}

func TestJoin_ErrorMapping(t *testing.T) {
	a := newApp(t, nil)
	owner := a.onboarded(t, "a@local", "18_plus")
	b := a.onboarded(t, "b@local", "18_plus")
	c := a.onboarded(t, "c@local", "18_plus")
	d := a.onboarded(t, "d@local", "18_plus")
	minor := a.onboarded(t, "m@local", "13_17")
	fresh := a.bare(t, "n@local")
	extra1 := a.extraCode(t, owner, "XTRA2345")
	extra2 := a.extraCode(t, owner, "XTRB2345")
	if w := a.join(b, owner.code); w.Code != 200 {
		t.Fatalf("b joins: %d %s", w.Code, w.Body.String())
	}
	if w := a.join(c, extra1); w.Code != 200 {
		t.Fatalf("c joins: %d %s", w.Code, w.Body.String())
	}

	cases := []struct {
		name   string
		who    member
		code   string
		before func()
		status int
		want   string
	}{
		{name: "unknown code", who: d, code: "ZZZZ2345", status: 404, want: "CODE_INVALID"},
		{name: "already member", who: b, code: extra2, status: 409, want: "ALREADY_MEMBER"},
		{name: "third room", who: c, code: b.code, status: 409, want: "ROOM_LIMIT"},
		{name: "age", who: minor, code: extra2, status: 403, want: "AGE_RESTRICTED"},
		{name: "not onboarded", who: fresh, code: extra2, status: 403, want: "ONBOARDING_REQUIRED"},
		{name: "no token", who: member{}, code: extra2, status: 401, want: "UNAUTHORIZED"},
		{name: "full", who: d, code: extra2, status: 409, want: "ROOM_FULL",
			before: func() { a.exec(t, `update rooms set member_count=capacity where id=$1`, owner.roomID) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.before != nil {
				tc.before()
			}
			w := a.join(tc.who, tc.code)
			e := errOf(t, w)
			if w.Code != tc.status || e["code"] != tc.want {
				t.Fatalf("status %d body %s", w.Code, w.Body.String())
			}
			if tc.want == "ALREADY_MEMBER" {
				det, _ := e["details"].(map[string]any)
				if det["room_id"] != owner.roomID {
					t.Fatalf("details = %v", e["details"])
				}
			}
		})
	}
}

func TestJoin_BadBodies(t *testing.T) {
	a := newApp(t, nil)
	owner := a.onboarded(t, "a@local", "18_plus")
	b := a.onboarded(t, "b@local", "18_plus")
	for name, body := range map[string]string{
		"empty object": `{}`,
		"null":         `{"code":null}`,
		"number":       `{"code":123}`,
		"not json":     `not json`,
		"empty body":   ``,
	} {
		w := call(a.r, "POST", joinPath, body, b.token)
		if w.Code != 400 || errOf(t, w)["code"] != "VALIDATION_FAILED" {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	// Had the bad bodies counted as failures, the cooldown would start here.
	for i := 0; i < 4; i++ {
		if w := a.join(b, "ZZZZ2345"); w.Code != 404 {
			t.Fatalf("wrong code %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	if w := a.join(b, owner.code); w.Code != 200 {
		t.Fatalf("valid code: %d %s", w.Code, w.Body.String())
	}
}

func TestJoin_GarbageCodes(t *testing.T) {
	a := newApp(t, nil)
	b := a.onboarded(t, "b@local", "18_plus")
	for name, code := range map[string]string{
		"full width": "ＡＢＣＤ２３４５",
		"10KB":       strings.Repeat("A", 10<<10),
		"blank":      "  ",
		"bad char":   "abcd-0efg",
	} {
		w := a.join(b, code)
		if w.Code != 404 || errOf(t, w)["code"] != "CODE_INVALID" {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
}

func TestJoin_CooldownAfterFiveWrongCodes(t *testing.T) {
	a := newApp(t, nil)
	owner := a.onboarded(t, "a@local", "18_plus")
	b := a.onboarded(t, "b@local", "18_plus")
	for i := 0; i < 5; i++ {
		if w := a.join(b, "ZZZZ2345"); w.Code != 404 {
			t.Fatalf("wrong code %d: %d", i, w.Code)
		}
	}
	w := a.join(b, owner.code)
	e := errOf(t, w)
	if w.Code != 429 || e["code"] != "CODE_COOLDOWN" {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	secs, _ := e["details"].(map[string]any)["retry_after_seconds"].(float64)
	if secs <= 0 || secs > 1800 || secs != float64(int(secs)) {
		t.Fatalf("retry_after_seconds = %v", e["details"])
	}
}

func TestJoin_IPRateLimited(t *testing.T) {
	a := newApp(t, nil)
	c := a.onboarded(t, "c@local", "18_plus")
	// Bare-user attempts count towards the IP limit but never cool down.
	n := a.bare(t, "n@local")
	for i := 0; i < 30; i++ {
		if w := a.join(n, "ZZZZ2345"); w.Code != 403 {
			t.Fatalf("attempt %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	w := a.join(c, "ZZZZ2345")
	if w.Code != 429 || errOf(t, w)["code"] != "RATE_LIMITED" {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	if ra := w.Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("Retry-After = %q", ra)
	}
}

func TestJoin_DisabledAccount(t *testing.T) {
	a := newApp(t, nil)
	owner := a.onboarded(t, "a@local", "18_plus")
	b := a.onboarded(t, "b@local", "18_plus")
	a.exec(t, `update users set status='banned' where id=$1`, b.id)
	w := a.join(b, owner.code)
	if w.Code != 403 || errOf(t, w)["code"] != "ACCOUNT_DISABLED" {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
}

func TestJoin_WorksWhenRedisDown(t *testing.T) {
	a := newApp(t, ratelimit.NewFallback(failingCounter{}, ratelimit.NewMemory(time.Now), nil))
	owner := a.onboarded(t, "a@local", "18_plus")
	b := a.onboarded(t, "b@local", "18_plus")
	if w := a.join(b, owner.code); w.Code != 200 {
		t.Fatalf("join: %d %s", w.Code, w.Body.String())
	}
}

func TestRoomsMine_HTTP(t *testing.T) {
	a := newApp(t, nil)
	if w := call(a.r, "GET", "/api/v1/rooms/mine", "", ""); w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}
	fresh := a.bare(t, "n@local")
	w := call(a.r, "GET", "/api/v1/rooms/mine", "", fresh.token)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"data":[]`) {
		t.Fatalf("bare user: %d %s", w.Code, w.Body.String())
	}

	owner := a.onboarded(t, "a@local", "18_plus")
	b := a.onboarded(t, "b@local", "18_plus")
	if w := a.join(b, owner.code); w.Code != 200 {
		t.Fatalf("join: %d %s", w.Code, w.Body.String())
	}
	w = call(a.r, "GET", "/api/v1/rooms/mine", "", b.token)
	if w.Code != 200 {
		t.Fatalf("mine: %d %s", w.Code, w.Body.String())
	}
	list, _ := decode(t, w)["data"].([]any)
	if len(list) != 2 {
		t.Fatalf("rooms = %s", w.Body.String())
	}
	own, other := list[0].(map[string]any), list[1].(map[string]any)
	if own["id"] != b.roomID || own["is_owner"] != true || own["member_count"] != float64(1) {
		t.Fatalf("own room = %v", own)
	}
	if other["id"] != owner.roomID || other["is_owner"] != false || other["member_count"] != float64(2) ||
		other["status"] != "open" || other["capacity"] == nil {
		t.Fatalf("joined room = %v", other)
	}
	members, _ := other["members"].([]any)
	if len(members) != 2 {
		t.Fatalf("members = %v", other["members"])
	}
	for _, m := range members {
		mm := m.(map[string]any)
		for _, k := range []string{"user_id", "nickname", "pet_type", "seat_no", "role"} {
			if _, ok := mm[k]; !ok {
				t.Fatalf("member lacks %s: %v", k, mm)
			}
		}
	}
}
