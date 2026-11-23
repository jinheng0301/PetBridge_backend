package handler_test

import (
	"strings"
	"testing"
)

func TestBlockRoutes(t *testing.T) {
	a := newApp(t, nil)
	me := a.onboarded(t, "me@local", "18_plus")
	x := a.onboarded(t, "x@local", "18_plus")
	y := a.onboarded(t, "y@local", "18_plus")

	for _, target := range []member{x, y} {
		w := call(a.r, "POST", "/api/v1/blocks", `{"user_id":"`+strings.ToUpper(target.id)+`"}`, me.token)
		if w.Code != 204 || w.Body.Len() != 0 {
			t.Fatalf("block: %d %q", w.Code, w.Body.String())
		}
	}
	// Idempotent.
	if w := call(a.r, "POST", "/api/v1/blocks", `{"user_id":"`+x.id+`"}`, me.token); w.Code != 204 {
		t.Fatalf("repeat block: %d %s", w.Code, w.Body.String())
	}

	w := call(a.r, "GET", "/api/v1/blocks", "", me.token)
	if w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	items, ok := asMap(t, w)["data"].(map[string]any)["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("list body %s", w.Body.String())
	}
	got := map[string]bool{}
	for _, it := range items {
		m := it.(map[string]any)
		for _, k := range []string{"user_id", "nickname", "pet_type", "blocked_at"} {
			if _, ok := m[k]; !ok {
				t.Fatalf("item missing %s: %v", k, m)
			}
		}
		got[m["user_id"].(string)] = true
	}
	if !got[x.id] || !got[y.id] {
		t.Fatalf("items %v", got)
	}

	if w := call(a.r, "DELETE", "/api/v1/blocks/"+strings.ToUpper(x.id), "", me.token); w.Code != 204 || w.Body.Len() != 0 {
		t.Fatalf("unblock: %d %q", w.Code, w.Body.String())
	}
	if w := call(a.r, "DELETE", "/api/v1/blocks/"+x.id, "", me.token); w.Code != 204 {
		t.Fatalf("repeat unblock: %d %s", w.Code, w.Body.String())
	}
	w = call(a.r, "GET", "/api/v1/blocks", "", me.token)
	if items := asMap(t, w)["data"].(map[string]any)["items"].([]any); len(items) != 1 {
		t.Fatalf("after unblock %s", w.Body.String())
	}

	// Empty list is [] not null.
	w = call(a.r, "GET", "/api/v1/blocks", "", x.token)
	if !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatalf("empty list %s", w.Body.String())
	}

	// Validation.
	for _, body := range []string{`{"user_id":"` + me.id + `"}`, `{"user_id":"nope"}`, `{}`} {
		e := assertErr(t, call(a.r, "POST", "/api/v1/blocks", body, me.token), 400, "VALIDATION_FAILED")
		if f, _ := e["details"].(map[string]any)["fields"].([]any); len(f) != 1 || f[0] != "user_id" {
			t.Fatalf("details %v", e)
		}
	}
	assertErr(t, call(a.r, "POST", "/api/v1/blocks", `not json`, me.token), 400, "VALIDATION_FAILED")
	assertErr(t, call(a.r, "DELETE", "/api/v1/blocks/nope", "", me.token), 400, "VALIDATION_FAILED")
	assertErr(t, call(a.r, "GET", "/api/v1/blocks", "", ""), 401, "UNAUTHORIZED")
}

func TestBlockRoutes_Limit(t *testing.T) {
	a := newApp(t, nil)
	me := a.onboarded(t, "me@local", "18_plus")
	x := a.onboarded(t, "x@local", "18_plus")
	a.exec(t, `with u as (insert into users(onboarded_at) select now() from generate_series(1, 500) returning id)
		insert into blocks(blocker_id, blocked_id) select $1, id from u`, me.id)
	assertErr(t, call(a.r, "POST", "/api/v1/blocks", `{"user_id":"`+x.id+`"}`, me.token), 409, "BLOCK_LIMIT")
}

func TestBlockRoutes_OnboardingRequired(t *testing.T) {
	a := newApp(t, nil)
	bare := a.bare(t, "bare@local")
	x := a.onboarded(t, "x@local", "18_plus")
	assertErr(t, call(a.r, "POST", "/api/v1/blocks", `{"user_id":"`+x.id+`"}`, bare.token), 403, "ONBOARDING_REQUIRED")
	assertErr(t, call(a.r, "DELETE", "/api/v1/blocks/"+x.id, "", bare.token), 403, "ONBOARDING_REQUIRED")
	assertErr(t, call(a.r, "GET", "/api/v1/blocks", "", bare.token), 403, "ONBOARDING_REQUIRED")
}
