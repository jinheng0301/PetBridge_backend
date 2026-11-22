package handler_test

import (
	"fmt"
	"testing"
)

func polishPath(id string) string { return "/api/v1/deliveries/" + id + "/polish" }

// sentDelivery has owner send one text message to the room and returns b's delivery id.
func sentDelivery(t *testing.T, a *app, owner, b member, cmid string) string {
	t.Helper()
	w := call(a.r, "POST", msgPath(owner.roomID),
		sendBody(map[string]any{"client_msg_id": cmid, "kind": "text", "text": "明天下午三点开会，记得带资料"}), owner.token)
	if w.Code != 202 {
		t.Fatalf("send: %d %s", w.Code, w.Body.String())
	}
	var id string
	if err := a.pool.QueryRow(bg, `select d.id::text from deliveries d join messages m on m.id=d.message_id
		where m.client_msg_id=$1 and d.receiver_id=$2`, cmid, b.id).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func roomPair(t *testing.T, a *app) (member, member) {
	t.Helper()
	owner := a.onboarded(t, "a@local", "18_plus")
	b := a.onboarded(t, "b@local", "18_plus")
	if w := a.join(b, owner.code); w.Code != 200 {
		t.Fatalf("join: %d %s", w.Code, w.Body.String())
	}
	return owner, b
}

func TestChatHandler_Polish(t *testing.T) {
	a := newApp(t, nil)
	owner, b := roomPair(t, a)
	did := sentDelivery(t, a, owner, b, "c-1")

	w := call(a.r, "POST", polishPath(did), "", b.token)
	if w.Code != 202 {
		t.Fatalf("polish: %d %s", w.Code, w.Body.String())
	}
	d := data(t, w)["delivery"].(map[string]any)
	if d["id"] != did || d["polish_status"] != "pending" {
		t.Fatalf("delivery = %v", d)
	}

	if w := call(a.r, "POST", polishPath(did), "", owner.token); w.Code != 404 || errCode(t, w) != "DELIVERY_NOT_FOUND" {
		t.Fatalf("other's delivery: %d %s", w.Code, w.Body.String())
	}
	if w := call(a.r, "POST", polishPath("not-a-uuid"), "", b.token); w.Code != 404 || errCode(t, w) != "DELIVERY_NOT_FOUND" {
		t.Fatalf("bad id: %d %s", w.Code, w.Body.String())
	}

	notReady := sentDelivery(t, a, owner, b, "c-2")
	a.exec(t, `update deliveries set translation_status='pending', base_text=null where id=$1`, notReady)
	if w := call(a.r, "POST", polishPath(notReady), "", b.token); w.Code != 409 || errCode(t, w) != "POLISH_NOT_READY" {
		t.Fatalf("not ready: %d %s", w.Code, w.Body.String())
	}
	if w := call(a.r, "POST", polishPath(did), "", ""); w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}

	// 6 per minute: polishes 2..6 pass (the first was above), the 7th is refused with Retry-After.
	for i := 0; i < 5; i++ {
		id := sentDelivery(t, a, owner, b, fmt.Sprintf("m-%d", i))
		if w := call(a.r, "POST", polishPath(id), "", b.token); w.Code != 202 {
			t.Fatalf("polish %d: %d %s", i+2, w.Code, w.Body.String())
		}
	}
	id := sentDelivery(t, a, owner, b, "m-last")
	w = call(a.r, "POST", polishPath(id), "", b.token)
	if w.Code != 429 || errCode(t, w) != "QUOTA_EXCEEDED" || w.Header().Get("Retry-After") == "" {
		t.Fatalf("7th: %d %s retry=%q", w.Code, w.Body.String(), w.Header().Get("Retry-After"))
	}
	if r := errOf(t, w)["details"].(map[string]any)["reason"]; r != "minute_limit" {
		t.Fatalf("reason = %v", r)
	}
}

func TestChatHandler_PolishDailyQuota(t *testing.T) {
	a := newAppQuota(t, nil, 0)
	owner, b := roomPair(t, a)
	did := sentDelivery(t, a, owner, b, "c-1")
	w := call(a.r, "POST", polishPath(did), "", b.token)
	if w.Code != 429 || errCode(t, w) != "QUOTA_EXCEEDED" || w.Header().Get("Retry-After") != "" {
		t.Fatalf("daily: %d %s retry=%q", w.Code, w.Body.String(), w.Header().Get("Retry-After"))
	}
	if r := errOf(t, w)["details"].(map[string]any)["reason"]; r != "daily_quota" {
		t.Fatalf("reason = %v", r)
	}
}

func TestChatHandler_ListLimitZero(t *testing.T) {
	a := newApp(t, nil)
	owner := a.onboarded(t, "a@local", "18_plus")
	w := call(a.r, "GET", msgPath(owner.roomID)+"?limit=0", "", owner.token)
	if w.Code != 400 || errCode(t, w) != "VALIDATION_FAILED" {
		t.Fatalf("limit=0: %d %s", w.Code, w.Body.String())
	}
	fields, _ := errOf(t, w)["details"].(map[string]any)["fields"].([]any)
	if len(fields) != 1 || fields[0] != "limit" {
		t.Fatalf("fields = %v", fields)
	}
}
