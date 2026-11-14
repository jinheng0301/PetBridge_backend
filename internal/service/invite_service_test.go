package service

import (
	"testing"
)

func TestNewCode_FormatAndCharset(t *testing.T) {
	for i := 0; i < 1000; i++ {
		c, err := NewCode()
		if err != nil {
			t.Fatal(err)
		}
		if !codeRE.MatchString(c) {
			t.Fatalf("bad code %q", c)
		}
	}
}

func TestNewCode_NoObviousCollisions(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		c, _ := NewCode()
		if seen[c] {
			t.Fatalf("duplicate code %q", c)
		}
		seen[c] = true
	}
}

func TestAlphabet_Has32DistinctChars(t *testing.T) {
	if len(Alphabet) != 32 {
		t.Fatalf("len = %d", len(Alphabet))
	}
	seen := map[rune]bool{}
	for _, r := range Alphabet {
		if seen[r] {
			t.Fatalf("duplicate %q", r)
		}
		seen[r] = true
	}
	if Alphabet != "23456789ABCDEFGHJKLMNPQRSTUVWXYZ" {
		t.Fatalf("alphabet = %q", Alphabet)
	}
}
