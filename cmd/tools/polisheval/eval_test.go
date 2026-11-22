package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jinheng0301/petbridge/pkg/llm"
)

func TestLoadCases(t *testing.T) {
	in := `{"id":"a","tgt":"zh-Hans","src":"ja","category":"request","source":"お願い","base":"拜托了"}
{"id":"b","tgt":"en","src":"en","category":"short","source":"","base":"ok"}
`
	cs, err := LoadCases(strings.NewReader(in))
	if err != nil || len(cs) != 2 || cs[0].Base != "拜托了" || cs[1].Src != "en" {
		t.Fatalf("got %+v %v", cs, err)
	}
	_, err = LoadCases(strings.NewReader(`{"id":"a","tgt":"en","src":"en","category":"short","base":"x"}` + "\n{bad\n"))
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("want line 2 error, got %v", err)
	}
}

// stubProvider scripts Polish by the base text.
type stubProvider struct{ calls atomic.Int32 }

func (p *stubProvider) Name() string { return "stub" }
func (p *stubProvider) Translate(context.Context, llm.TranslateRequest) (llm.Result, error) {
	return llm.Result{}, nil
}
func (p *stubProvider) Polish(_ context.Context, req llm.PolishRequest) (llm.Result, error) {
	p.calls.Add(1)
	switch {
	case strings.Contains(req.Text, "3点"):
		return llm.Result{Text: strings.ReplaceAll(req.Text, "3点", "15:00"), Model: "stub"}, nil
	case strings.Contains(req.Text, "坏"):
		return llm.Result{}, llm.ErrBadOutput
	}
	return llm.Result{Text: req.Text + "呀", Model: "stub", Usage: llm.Usage{InputTokens: 10, OutputTokens: 2}}, nil
}

func TestRun_StatusPerCase(t *testing.T) {
	cases := []Case{
		{ID: "1", Tgt: "zh-Hans", Src: "ja", Category: "numbers_time", Base: "明天下午3点开会"},
		{ID: "2", Tgt: "zh-Hans", Src: "ja", Category: "negative", Base: "这个东西坏掉了真烦"},
		{ID: "3", Tgt: "zh-Hans", Src: "ja", Category: "request", Base: "能帮我带一下资料吗"},
		{ID: "4", Tgt: "zh-Hans", Src: "ja", Category: "short", Base: "好的！"},
	}
	p := &stubProvider{}
	rows := Run(context.Background(), p, cases, []string{"ISTJ", "ENFP"}, Options{Timeout: time.Second, Concurrency: 2})
	if len(rows) != 8 {
		t.Fatalf("rows %d, want 8", len(rows))
	}
	want := map[string]string{"1": "violation:facts", "2": "error:bad_output", "3": "pass", "4": "short"}
	for _, r := range rows {
		if r.Status != want[r.ID] {
			t.Errorf("case %s %s: status %q, want %q", r.ID, r.MBTI, r.Status, want[r.ID])
		}
		if r.ID == "4" && r.Polished != r.Base {
			t.Errorf("short case polished %q", r.Polished)
		}
	}
	if n := p.calls.Load(); n != 6 {
		t.Errorf("provider calls %d, want 6 (short case skipped)", n)
	}
}

func TestSummarize(t *testing.T) {
	rows := []Row{
		{Case: Case{Tgt: "en"}, Status: "pass"},
		{Case: Case{Tgt: "en"}, Status: "violation:length"},
		{Case: Case{Tgt: "en"}, Status: "short"},
	}
	s := Summarize(rows)
	l := s.Langs["en"]
	if l.Total != 3 || l.Short != 1 || l.Pass != 1 || l.PassRate() != 0.5 || l.Fails["violation:length"] != 1 {
		t.Fatalf("got %+v", l)
	}
}

func TestWriteCSV(t *testing.T) {
	var b bytes.Buffer
	if err := WriteCSV(&b, []Row{{Case: Case{ID: "x", Base: "好"}, MBTI: "ISTJ", Status: "short"}}); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(b.Bytes(), []byte("\xEF\xBB\xBF")) {
		t.Fatal("missing BOM")
	}
	recs, err := csv.NewReader(bytes.NewReader(b.Bytes()[3:])).ReadAll()
	if err != nil || len(recs) != 2 || len(recs[0]) != 11 {
		t.Fatalf("recs %v %v", recs, err)
	}
}

func TestEvalSet(t *testing.T) {
	f, err := os.Open("testdata/polish_eval.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cases, err := LoadCases(f)
	if err != nil {
		t.Fatal(err)
	}
	perLang := map[string]int{}
	cats := map[string]map[string]bool{}
	injection := map[string]int{}
	ids := map[string]bool{}
	for _, c := range cases {
		if ids[c.ID] {
			t.Errorf("duplicate id %s", c.ID)
		}
		ids[c.ID] = true
		perLang[c.Tgt]++
		if cats[c.Tgt] == nil {
			cats[c.Tgt] = map[string]bool{}
		}
		cats[c.Tgt][c.Category] = true
		if c.Category == "injection" {
			injection[c.Tgt]++
		}
		if c.Src == c.Tgt && c.Source != "" {
			t.Errorf("%s: same-language case must have empty source", c.ID)
		}
		if c.Src != c.Tgt && c.Source == "" {
			t.Errorf("%s: cross-language case needs a source", c.ID)
		}
		if !validCategory[c.Category] {
			t.Errorf("%s: unknown category %q", c.ID, c.Category)
		}
	}
	for _, lang := range []string{"ja", "ko", "zh-Hant", "zh-Hans", "en"} {
		if perLang[lang] < 40 {
			t.Errorf("%s: %d cases, want >= 40", lang, perLang[lang])
		}
		if len(cats[lang]) != len(validCategory) {
			t.Errorf("%s: %d categories, want %d", lang, len(cats[lang]), len(validCategory))
		}
		if injection[lang] < 3 {
			t.Errorf("%s: %d injection cases, want >= 3", lang, injection[lang])
		}
	}
}
