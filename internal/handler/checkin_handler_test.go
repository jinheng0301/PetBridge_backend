package handler_test

import (
	"strconv"
	"strings"
	"testing"
)

func TestCheckinRoute(t *testing.T) {
	a := newApp(t, nil)
	me := a.onboarded(t, "me@local", "18_plus")
	stranger := a.onboarded(t, "s@local", "18_plus")
	bare := a.bare(t, "bare@local")
	path := "/api/v1/rooms/" + me.roomID + "/checkin"

	w := call(a.r, "POST", path, "", me.token)
	if w.Code != 200 {
		t.Fatalf("first: %d %s", w.Code, w.Body.String())
	}
	d := asMap(t, w)["data"].(map[string]any)
	for _, k := range []string{"already", "energy_gained", "bonus_gained", "checkin_expires_at", "energy_total", "next_threshold", "unlocked"} {
		if _, ok := d[k]; !ok {
			t.Errorf("missing %s in %v", k, d)
		}
	}
	u, _ := d["unlocked"].([]any)
	if d["already"] != false || d["energy_gained"] != 10.0 || d["energy_total"] != 10.0 || len(u) != 1 || u[0] != "wall_clock" || len(d) != 7 {
		t.Fatalf("first %v", d)
	}

	w = call(a.r, "POST", path, "", me.token)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"already":true`) || !strings.Contains(w.Body.String(), `"unlocked":[]`) {
		t.Fatalf("repeat: %d %s", w.Code, w.Body.String())
	}

	if w := call(a.r, "POST", path, "", ""); w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}
	for name, c := range map[string]struct{ path, token string }{
		"stranger":      {path, stranger.token},
		"not onboarded": {path, bare.token},
		"malformed":     {"/api/v1/rooms/abc/checkin", me.token},
		"unknown":       {"/api/v1/rooms/00000000-0000-0000-0000-000000000000/checkin", me.token},
	} {
		if w := call(a.r, "POST", c.path, "", c.token); w.Code != 404 || errCode(t, w) != "ROOM_NOT_FOUND" {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
}

func TestCheckinRoute_RateLimited(t *testing.T) {
	a := newApp(t, nil)
	me := a.onboarded(t, "me@local", "18_plus")
	path := "/api/v1/rooms/" + me.roomID + "/checkin"
	for i := 0; i < 10; i++ {
		if w := call(a.r, "POST", path, "", me.token); w.Code != 200 {
			t.Fatalf("call %d: %d %s", i+1, w.Code, w.Body.String())
		}
	}
	w := call(a.r, "POST", path, "", me.token)
	if w.Code != 429 || errCode(t, w) != "RATE_LIMITED" {
		t.Fatalf("11th: %d %s", w.Code, w.Body.String())
	}
	if secs, err := strconv.Atoi(w.Header().Get("Retry-After")); err != nil || secs < 1 || secs > 60 {
		t.Fatalf("Retry-After = %q", w.Header().Get("Retry-After"))
	}
	if w := call(a.r, "GET", "/api/v1/rooms/"+me.roomID+"/decor", "", me.token); w.Code != 200 {
		t.Fatalf("decor is limited too: %d", w.Code)
	}
}

func TestDecorRoute(t *testing.T) {
	a := newApp(t, nil)
	me := a.onboarded(t, "me@local", "18_plus")
	stranger := a.onboarded(t, "s@local", "18_plus")
	path := "/api/v1/rooms/" + me.roomID + "/decor"
	if w := call(a.r, "POST", "/api/v1/rooms/"+me.roomID+"/checkin", "", me.token); w.Code != 200 {
		t.Fatalf("check-in: %d %s", w.Code, w.Body.String())
	}
	w := call(a.r, "GET", path, "", me.token)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	d := asMap(t, w)["data"].(map[string]any)
	items := d["items"].([]any)
	first, second := items[0].(map[string]any), items[1].(map[string]any)
	if d["energy_total"] != 10.0 || d["next_threshold"] != 40.0 || len(items) != 12 ||
		first["item_id"] != "wall_clock" || first["threshold"] != 10.0 || first["unlocked_at"] == nil {
		t.Fatalf("%v", d)
	}
	if v, ok := second["unlocked_at"]; !ok || v != nil {
		t.Fatalf("locked item %v", second)
	}
	if w := call(a.r, "GET", path, "", stranger.token); w.Code != 404 || errCode(t, w) != "ROOM_NOT_FOUND" {
		t.Fatalf("stranger: %d %s", w.Code, w.Body.String())
	}
	if w := call(a.r, "GET", path, "", ""); w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}
}
