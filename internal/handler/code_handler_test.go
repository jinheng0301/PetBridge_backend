package handler_test

import (
	"strings"
	"testing"
)

func TestCodes_HTTP(t *testing.T) {
	a := newApp(t, nil)
	if w := call(a.r, "GET", "/api/v1/codes", "", ""); w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}
	fresh := a.bare(t, "n@local")
	w := call(a.r, "GET", "/api/v1/codes", "", fresh.token)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"data":[]`) {
		t.Fatalf("bare user: %d %s", w.Code, w.Body.String())
	}

	owner := a.onboarded(t, "a@local", "18_plus")
	a.exec(t, `insert into invite_codes(code, room_id, issued_to, source, expires_at)
		values('XPRD2345', $1, $2, 'owner_regen', now() - interval '1 hour')`, owner.roomID, owner.id)
	w = call(a.r, "GET", "/api/v1/codes", "", owner.token)
	if w.Code != 200 {
		t.Fatalf("codes: %d %s", w.Code, w.Body.String())
	}
	list, _ := decode(t, w)["data"].([]any)
	if len(list) != 2 {
		t.Fatalf("codes = %s", w.Body.String())
	}
	byCode := map[string]map[string]any{}
	for _, it := range list {
		m := it.(map[string]any)
		byCode[m["code"].(string)] = m
	}
	signup := byCode[owner.code]
	if signup == nil || signup["source"] != "signup" || signup["status"] != "unused" || signup["expires_at"] == nil {
		t.Fatalf("signup = %v", signup)
	}
	room, _ := signup["room"].(map[string]any)
	if room["id"] != owner.roomID || room["member_count"] != float64(1) || room["capacity"] == nil {
		t.Fatalf("room = %v", signup["room"])
	}
	if old := byCode["XPRD2345"]; old == nil || old["status"] != "expired" {
		t.Fatalf("expired code = %v", old)
	}
}
