package service

import "testing"

func TestPetForMBTI_All16(t *testing.T) {
	want := map[string]string{
		"INTJ": "lone_leopard", "ENTJ": "leader_lion",
		"INTP": "thinker_owl", "ENTP": "clever_fox",
		"INFJ": "guardian_deer", "ENFJ": "guide_dolphin",
		"INFP": "healing_rabbit", "ENFP": "happy_shiba",
		"ISTJ": "steady_beaver", "ESTJ": "order_sheepdog",
		"ISFJ": "guardian_penguin", "ESFJ": "warm_otter",
		"ISTP": "calm_blackcat", "ESTP": "daring_falcon",
		"ISFP": "easygoing_alpaca", "ESFP": "party_parrot",
	}
	seen := map[string]bool{}
	for mbti, slug := range want {
		got, ok := PetForMBTI(mbti)
		if !ok || got != slug {
			t.Errorf("%s: got %q ok=%v, want %q", mbti, got, ok, slug)
		}
		if seen[got] {
			t.Errorf("duplicate slug %q", got)
		}
		seen[got] = true
	}
	if len(want) != 16 {
		t.Fatalf("table has %d entries", len(want))
	}
}

func TestPetForMBTI_Unknown(t *testing.T) {
	for _, in := range []string{"XXXX", "intj", "", "INT"} {
		if _, ok := PetForMBTI(in); ok {
			t.Errorf("%q should not map", in)
		}
	}
}
