package service

import "testing"

func TestPhrases_CompleteAndStable(t *testing.T) {
	wantIDs := []string{"good_morning", "good_night", "thank_you", "sorry", "ok", "see_you", "congrats", "take_care"}
	got := Phrases()
	if len(got) != len(wantIDs) {
		t.Fatalf("phrases = %d, want %d", len(got), len(wantIDs))
	}
	for i, p := range got {
		if p.ID != wantIDs[i] {
			t.Fatalf("phrase %d id = %q, want %q", i, p.ID, wantIDs[i])
		}
		for _, lang := range PhraseLangs {
			if p.Texts[lang] == "" {
				t.Errorf("phrase %s has no %s text", p.ID, lang)
			}
		}
		if len(p.Texts) != len(PhraseLangs) {
			t.Errorf("phrase %s has %d texts, want %d", p.ID, len(p.Texts), len(PhraseLangs))
		}
	}
	if s, ok := PhraseText("good_morning", "ja"); !ok || s == "" {
		t.Fatalf("good_morning/ja = %q, %v", s, ok)
	}
	if _, ok := PhraseText("nope", "ja"); ok {
		t.Fatal("unknown id must report false")
	}
	if _, ok := PhraseText("good_morning", "fr"); ok {
		t.Fatal("unknown lang must report false")
	}
}
