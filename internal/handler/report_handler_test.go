package handler_test

import (
	"encoding/json"
	"strconv"
	"testing"
)

func TestReportRoute(t *testing.T) {
	a := newApp(t, nil)
	me := a.onboarded(t, "me@local", "18_plus")
	x := a.onboarded(t, "x@local", "18_plus")
	bare := a.bare(t, "bare@local")
	if w := a.join(x, me.code); w.Code != 200 {
		t.Fatalf("join: %d %s", w.Code, w.Body.String())
	}
	body := func(m map[string]any) string { b, _ := json.Marshal(m); return string(b) }
	ok := map[string]any{"target_user_id": x.id, "room_id": me.roomID, "reason": "harassment", "note": "rude"}

	if w := call(a.r, "POST", "/api/v1/reports", body(ok), ""); w.Code != 401 {
		t.Fatalf("anon: %d", w.Code)
	}
	if w := call(a.r, "POST", "/api/v1/reports", body(ok), bare.token); w.Code != 403 || errCode(t, w) != "ONBOARDING_REQUIRED" {
		t.Fatalf("bare: %d %s", w.Code, w.Body.String())
	}

	w := call(a.r, "POST", "/api/v1/reports", body(ok), me.token)
	if w.Code != 201 {
		t.Fatalf("report: %d %s", w.Code, w.Body.String())
	}
	d := asMap(t, w)["data"].(map[string]any)
	id, _ := d["id"].(string)
	if id == "" || d["created_at"] == nil {
		t.Fatalf("body %s", w.Body.String())
	}
	// Repeating returns the same open report.
	w = call(a.r, "POST", "/api/v1/reports", body(ok), me.token)
	if w.Code != 201 || asMap(t, w)["data"].(map[string]any)["id"] != id {
		t.Fatalf("repeat: %d %s", w.Code, w.Body.String())
	}

	bad := map[string]any{"target_user_id": "x", "room_id": me.roomID, "reason": "nope"}
	w = call(a.r, "POST", "/api/v1/reports", body(bad), me.token)
	if w.Code != 400 || errCode(t, w) != "VALIDATION_FAILED" {
		t.Fatalf("bad: %d %s", w.Code, w.Body.String())
	}
	fields := asMap(t, w)["error"].(map[string]any)["details"].(map[string]any)["fields"].([]any)
	if len(fields) != 2 {
		t.Fatalf("fields %v", fields)
	}
	if w := call(a.r, "POST", "/api/v1/reports", "{", me.token); w.Code != 400 {
		t.Fatalf("garbage: %d", w.Code)
	}

	// Twenty reports today (none open) exhaust the daily limit.
	a.exec(t, `delete from reports`)
	a.exec(t, `insert into reports(reporter_id, target_id, room_id, reason, status)
		select $1, $2, $3, 'spam', 'dismissed' from generate_series(1,20)`, me.id, x.id, me.roomID)
	w = call(a.r, "POST", "/api/v1/reports", body(ok), me.token)
	if w.Code != 429 || errCode(t, w) != "RATE_LIMITED" {
		t.Fatalf("limit: %d %s", w.Code, w.Body.String())
	}
	if secs, err := strconv.Atoi(w.Header().Get("Retry-After")); err != nil || secs < 1 || secs > 86400 {
		t.Fatalf("Retry-After = %q", w.Header().Get("Retry-After"))
	}
}
