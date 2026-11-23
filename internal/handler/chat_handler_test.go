package handler_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// nopQueue accepts every enqueue; the handler tests check HTTP mapping only.
type nopQueue struct{}

func (nopQueue) EnqueueTranslate(context.Context, string, string) error { return nil }
func (nopQueue) EnqueuePolish(context.Context, string, string) error    { return nil }

// nopMeter reports no AI spend.
type nopMeter struct{}

func (nopMeter) Add(context.Context, string, int64) error     { return nil }
func (nopMeter) Total(context.Context, string) (int64, error) { return 0, nil }

func msgPath(roomID string) string { return "/api/v1/rooms/" + roomID + "/messages" }

func sendBody(fields map[string]any) string {
	b, _ := json.Marshal(fields)
	return string(b)
}

func TestChatHandler_Send(t *testing.T) {
	a := newApp(t, nil)
	owner := a.onboarded(t, "a@local", "18_plus")
	b := a.onboarded(t, "b@local", "18_plus")
	if w := a.join(b, owner.code); w.Code != 200 {
		t.Fatalf("join: %d %s", w.Code, w.Body.String())
	}

	w := call(a.r, "POST", msgPath(owner.roomID),
		sendBody(map[string]any{"client_msg_id": "c-1", "kind": "text", "text": "hello"}), owner.token)
	if w.Code != 202 {
		t.Fatalf("send: %d %s", w.Code, w.Body.String())
	}
	d := data(t, w)
	if d["client_msg_id"] != "c-1" || d["room_id"] != owner.roomID || d["text"] != "hello" {
		t.Fatalf("data = %v", d)
	}
	if v, ok := d["delivery"]; !ok || v != nil {
		t.Fatalf("delivery = %v (present %v), want null", v, ok)
	}

	if w := call(a.r, "POST", msgPath(owner.roomID), `{"client_msg_id":`, owner.token); w.Code != 400 ||
		errCode(t, w) != "VALIDATION_FAILED" {
		t.Fatalf("bad json: %d %s", w.Code, w.Body.String())
	}

	w = call(a.r, "POST", msgPath(owner.roomID),
		sendBody(map[string]any{"client_msg_id": "c-2", "kind": "text", "text": strings.Repeat("あ", 501)}), owner.token)
	if w.Code != 400 || errCode(t, w) != "VALIDATION_FAILED" {
		t.Fatalf("long text: %d %s", w.Code, w.Body.String())
	}
	fields, _ := errOf(t, w)["details"].(map[string]any)["fields"].([]any)
	if len(fields) != 1 || fields[0] != "text" {
		t.Fatalf("fields = %v", fields)
	}

	// b is not seated in a stranger's room.
	stranger := a.onboarded(t, "c@local", "18_plus")
	w = call(a.r, "POST", msgPath(stranger.roomID),
		sendBody(map[string]any{"client_msg_id": "c-3", "kind": "text", "text": "hi"}), b.token)
	if w.Code != 404 || errCode(t, w) != "ROOM_NOT_FOUND" {
		t.Fatalf("not seated: %d %s", w.Code, w.Body.String())
	}

	if w := call(a.r, "POST", msgPath(owner.roomID), `{}`, ""); w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}
	if strings.Contains(a.logs.String(), "hello") {
		t.Fatal("message text found in logs")
	}
}

func TestChatHandler_List(t *testing.T) {
	a := newApp(t, nil)
	owner := a.onboarded(t, "a@local", "18_plus")

	w := call(a.r, "GET", msgPath(owner.roomID), "", owner.token)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatalf("empty list: %d %s", w.Code, w.Body.String())
	}
	if _, ok := data(t, w)["has_more"].(bool); !ok {
		t.Fatalf("has_more not a bool: %s", w.Body.String())
	}

	if w := call(a.r, "POST", msgPath(owner.roomID),
		sendBody(map[string]any{"client_msg_id": "c-1", "kind": "text", "text": "hello"}), owner.token); w.Code != 202 {
		t.Fatalf("send: %d %s", w.Code, w.Body.String())
	}
	w = call(a.r, "GET", msgPath(owner.roomID)+"?limit=10", "", owner.token)
	if w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	d := data(t, w)
	items, ok := d["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["client_msg_id"] != "c-1" {
		t.Fatalf("items = %v", d["items"])
	}
	if d["has_more"] != false {
		t.Fatalf("has_more = %v", d["has_more"])
	}

	w = call(a.r, "GET", msgPath(owner.roomID)+"?limit=abc", "", owner.token)
	if w.Code != 400 || errCode(t, w) != "VALIDATION_FAILED" {
		t.Fatalf("limit=abc: %d %s", w.Code, w.Body.String())
	}
	fields, _ := errOf(t, w)["details"].(map[string]any)["fields"].([]any)
	if len(fields) != 1 || fields[0] != "limit" {
		t.Fatalf("fields = %v", fields)
	}

	stranger := a.onboarded(t, "c@local", "18_plus")
	if w := call(a.r, "GET", msgPath(stranger.roomID), "", owner.token); w.Code != 404 ||
		errCode(t, w) != "ROOM_NOT_FOUND" {
		t.Fatalf("not seated: %d %s", w.Code, w.Body.String())
	}
}

func TestSend_ContentBlocked422(t *testing.T) {
	a := newApp(t, nil)
	owner := a.onboarded(t, "a@local", "18_plus")
	w := call(a.r, "POST", msgPath(owner.roomID),
		sendBody(map[string]any{"client_msg_id": "c-1", "kind": "text", "text": "zzsecret bad"}), owner.token)
	if w.Code != 422 || errCode(t, w) != "CONTENT_BLOCKED" {
		t.Fatalf("send: %d %s", w.Code, w.Body.String())
	}
	if _, ok := errOf(t, w)["details"]; ok {
		t.Fatalf("details present: %s", w.Body.String())
	}
	if n := count(t, a.pool, `select count(*) from messages`); n != 0 {
		t.Fatalf("messages = %d", n)
	}
	if strings.Contains(a.logs.String(), "zzsecret") {
		t.Fatal("message text found in logs")
	}
}
