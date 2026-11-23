package service

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jinheng0301/petbridge/pkg/wordfilter"
)

func testModerator() *WordlistModerator {
	return NewWordlistModerator(wordfilter.New([]wordfilter.Entry{
		{Term: "bad", Level: wordfilter.Block},
		{Term: "meh", Level: wordfilter.Review},
	}))
}

func TestCheckTranslated_MergeTable(t *testing.T) {
	cases := []struct {
		text, risk string
		verdict    ModVerdict
		model      ModVerdict
		source     string
	}{
		{"ok", "none", ModPass, ModPass, ""},
		{"ok", "review", ModReview, ModReview, ModSourceModel},
		{"ok", "block", ModBlock, ModBlock, ModSourceModel},
		{"meh", "block", ModBlock, ModBlock, ModSourceBoth},
		{"bad", "review", ModBlock, ModReview, ModSourceBoth},
		{"bad", "none", ModBlock, ModPass, ModSourceWordlist},
		{"ok", "weird", ModPass, ModPass, ""},
	}
	m := testModerator()
	for _, c := range cases {
		res, err := m.CheckTranslated(context.Background(), c.text, "en", c.risk)
		if err != nil {
			t.Fatal(err)
		}
		if res.Verdict != c.verdict || res.Model != c.model || res.Source != c.source {
			t.Errorf("%q/%q: got verdict=%d model=%d source=%q; want %d %d %q",
				c.text, c.risk, res.Verdict, res.Model, res.Source, c.verdict, c.model, c.source)
		}
	}
}

func TestCheckOutgoing_TermsReturned(t *testing.T) {
	res, err := testModerator().CheckOutgoing(context.Background(), "you are bad", "en")
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != ModBlock || res.Model != ModPass || res.Source != ModSourceWordlist {
		t.Fatalf("got %+v", res)
	}
	if len(res.Terms) != 1 || res.Terms[0] != "bad" {
		t.Fatalf("terms = %v", res.Terms)
	}
	clean, _ := testModerator().CheckOutgoing(context.Background(), "hello", "en")
	if clean.Verdict != ModPass || clean.Source != "" || len(clean.Terms) != 0 {
		t.Fatalf("clean = %+v", clean)
	}
}

func TestLoadModerator_EmbeddedListsParse(t *testing.T) {
	m, err := LoadModerator()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ja", "ko", "zh-Hant", "zh-Hans", "en"} {
		f, err := os.Open(filepath.Join("modwords", name+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		entries, err := wordfilter.ParseList(bufio.NewReader(f))
		f.Close()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(entries) < 30 {
			t.Errorf("%s: %d entries, want >= 30", name, len(entries))
		}
	}
	// One known block term from each embedded file, through the real loader.
	for name, text := range map[string]string{
		"en": "I think about suicide", "ja": "自殺したい", "ko": "자살 생각",
		"zh-Hans": "杀猪盘", "zh-Hant": "殺豬盤",
	} {
		res, err := m.CheckOutgoing(context.Background(), text, name)
		if err != nil || res.Verdict != ModBlock {
			t.Errorf("%s: %q got %+v err=%v, want Block", name, text, res, err)
		}
	}
}

func TestLoadModerator_NoFalsePositives(t *testing.T) {
	m, err := LoadModerator()
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"カロリー控えめのごはん", "我想死你了", "出口交易", "一支那个笔", "I spend my life here",
		"I want to diet", "跳楼价大甩卖", "パンくず", "私にやらせて", "송금해줬어", "ガイジンの友達",
	} {
		res, err := m.CheckOutgoing(context.Background(), text, "")
		if err != nil || res.Verdict != ModPass {
			t.Errorf("%q: got %+v err=%v, want Pass", text, res, err)
		}
	}
}
