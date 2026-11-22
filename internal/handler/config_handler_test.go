package handler_test

import "testing"

func TestConfigHandler_Phrases(t *testing.T) {
	a := newApp(t, nil)
	m := a.bare(t, "p@local")

	w := call(a.r, "GET", "/api/v1/config/phrases", "", m.token)
	if w.Code != 200 {
		t.Fatalf("no v: %d %s", w.Code, w.Body.String())
	}
	d := decode(t, w)["data"].(map[string]any)
	if d["version"] != "2" {
		t.Fatalf("version = %v", d["version"])
	}
	if ps, _ := d["phrases"].([]any); len(ps) != 8 {
		t.Fatalf("phrases = %v", d["phrases"])
	}

	w = call(a.r, "GET", "/api/v1/config/phrases?v=2", "", m.token)
	if w.Code != 304 || w.Body.Len() != 0 {
		t.Fatalf("v=2: %d body=%q", w.Code, w.Body.String())
	}
	// A client still caching version 1 must get the new texts.
	if w = call(a.r, "GET", "/api/v1/config/phrases?v=1", "", m.token); w.Code != 200 {
		t.Fatalf("v=1: %d", w.Code)
	}
	if w = call(a.r, "GET", "/api/v1/config/phrases", "", ""); w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}
}
