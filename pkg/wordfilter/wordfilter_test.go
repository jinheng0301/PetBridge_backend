package wordfilter

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestNormalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ＫＩＬＬ", "kill"},
		{"シネ", "しね"},
		{"a\u200bb", "ab"},
		{"a\u200c\u200d\u2060\ufeffb", "ab"},
	}
	for _, c := range cases {
		if got := Normalize(c.in); got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMatch_ASCIIWordBoundary(t *testing.T) {
	f := New([]Entry{{"ass", Block}})
	lvl, terms := f.Match("you ass!")
	if lvl != Block || len(terms) != 1 || terms[0] != "ass" {
		t.Fatalf("got %v %v", lvl, terms)
	}
	for _, s := range []string{"class", "assistant", "ass1"} {
		if lvl, terms := f.Match(s); lvl != None || len(terms) != 0 {
			t.Errorf("Match(%q) = %v %v, want None", s, lvl, terms)
		}
	}
}

func TestMatch_CJKIgnoresSeparators(t *testing.T) {
	f := New([]Entry{{"死了算了", Block}})
	for _, s := range []string{"死 了 算 了", "死-了_算.了", "死・了、算了", "死了算了"} {
		if lvl, _ := f.Match(s); lvl != Block {
			t.Errorf("Match(%q) = %v, want Block", s, lvl)
		}
	}
	if lvl, _ := f.Match("死了算"); lvl != None {
		t.Errorf("partial should not match")
	}
}

func TestMatch_Variants(t *testing.T) {
	f := New([]Entry{{"しね", Block}})
	if lvl, _ := f.Match("シネ"); lvl != Block {
		t.Errorf("katakana variant not matched")
	}
	f = New([]Entry{{"kill", Review}})
	if lvl, _ := f.Match("ＫＩＬＬ you"); lvl != Review {
		t.Errorf("fullwidth variant not matched")
	}
}

func TestMatch_HighestLevelAndTerms(t *testing.T) {
	f := New([]Entry{{"heck", Review}, {"死ね", Block}, {"heck", Review}, {"死ね", Review}})
	lvl, terms := f.Match("what the heck 死ね")
	if lvl != Block {
		t.Fatalf("level = %v", lvl)
	}
	if len(terms) != 2 || terms[0] != "heck" || terms[1] != "死ね" {
		t.Fatalf("terms = %v", terms)
	}

	var es []Entry
	var parts []string
	for i := 0; i < 12; i++ {
		w := fmt.Sprintf("词%c号", 'あ'+rune(i))
		es = append(es, Entry{w, Review})
		parts = append(parts, w)
	}
	f = New(es)
	lvl, terms = f.Match(strings.Join(parts, " "))
	if lvl != Review || len(terms) != 10 {
		t.Fatalf("got %v, %d terms", lvl, len(terms))
	}
}

func TestMatch_EmptyAndIgnored(t *testing.T) {
	f := New([]Entry{{"", Block}, {"\u200b", Block}, {" - ", Block}})
	if lvl, _ := f.Match("anything - here"); lvl != None {
		t.Errorf("empty terms must be ignored")
	}
}

func TestParseList(t *testing.T) {
	in := "# comment\n\nblock\t死ね\n  \nreview\tkill\r\n"
	es, err := ParseList(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 || es[0] != (Entry{"死ね", Block}) || es[1] != (Entry{"kill", Review}) {
		t.Fatalf("entries = %v", es)
	}
	_, err = ParseList(strings.NewReader("bad\tx\n"))
	if err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("err = %v", err)
	}
}

func TestMatch_Fast(t *testing.T) {
	var es []Entry
	for i := 0; i < 100; i++ {
		es = append(es, Entry{fmt.Sprintf("bad%dword", i), Review})
		es = append(es, Entry{fmt.Sprintf("坏词%d号", i), Block})
	}
	f := New(es)
	text := strings.Repeat("Hello 你好 wor ld こんにちは. ", 25)
	if n := len([]rune(text)); n < 500 {
		text += strings.Repeat("x", 500-n)
	}
	start := time.Now()
	for i := 0; i < 1000; i++ {
		f.Match(text)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("too slow: %v", d)
	}
}

func TestParseList_TrimsTerm(t *testing.T) {
	es, err := ParseList(strings.NewReader("block\tass \n"))
	if err != nil {
		t.Fatal(err)
	}
	if es[0].Term != "ass" {
		t.Fatalf("term = %q", es[0].Term)
	}
	f := New(es)
	if lvl, _ := f.Match("class"); lvl != None {
		t.Errorf("trailing space must not turn ass into a substring term")
	}
	if lvl, _ := f.Match("an ass"); lvl != Block {
		t.Errorf("expected match")
	}
}

func TestMatch_OverlappingSuffixTerms(t *testing.T) {
	f := New([]Entry{{"かさし", Review}, {"さし", Review}, {"さしと", Block}})
	lvl, terms := f.Match("うかさしとう")
	if lvl != Block || len(terms) != 3 {
		t.Fatalf("got %v %v", lvl, terms)
	}
	// classic "ushers" shape: suffix terms must all be reported, earliest start first.
	if terms[0] != "かさし" {
		t.Errorf("terms = %v", terms)
	}

	f = New([]Entry{{"abcd", Block}, {"bc", Review}})
	// bc inside abcd is rejected by the word boundary; a standalone bc is not.
	lvl, terms = f.Match("x abcd x")
	if lvl != Block || len(terms) != 1 || terms[0] != "abcd" {
		t.Fatalf("got %v %v", lvl, terms)
	}
	lvl, terms = f.Match("abcd bc")
	if lvl != Block || len(terms) != 2 || terms[0] != "abcd" || terms[1] != "bc" {
		t.Fatalf("got %v %v", lvl, terms)
	}
	// non-ASCII overlap goes through the loose automaton
	f = New([]Entry{{"你好吗", Block}, {"好吗", Review}})
	if _, terms = f.Match("你好吗"); len(terms) != 2 {
		t.Fatalf("terms = %v", terms)
	}
}

func TestMatch_ASCIITermAdjacentToCJK(t *testing.T) {
	f := New([]Entry{{"kill", Review}})
	if lvl, _ := f.Match("我要kill你"); lvl != Review {
		t.Errorf("CJK neighbours must not block an ASCII term")
	}
}

func TestMatch_ASCIIPhraseWholePhrase(t *testing.T) {
	f := New([]Entry{{"end my life", Block}, {"want to die", Block}})
	for _, s := range []string{"i want to end my life", "END   MY LIFE", "end\nmy life"} {
		if lv, _ := f.Match(s); lv != Block {
			t.Errorf("%q: want Block, got %v", s, lv)
		}
	}
	for _, s := range []string{"I spend my life here", "I want to diet", "bend my lifelong"} {
		if lv, terms := f.Match(s); lv != None {
			t.Errorf("%q: want None, got %v %v", s, lv, terms)
		}
	}
}
