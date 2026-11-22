// Command polisheval runs the polish eval set (backend TRD 10.3.9) against one
// provider: it prints the 10.3.7 check pass rate per language and writes a
// CSV for native speakers to score.
//
//	task polisheval -- -provider claude -lang zh-Hans -n 10 -types ISTJ,ENFP
//
// It reads only GEMINI_API_KEY, GEMINI_MODEL_POLISH, GEMINI_FREE_TIER,
// GEMINI_PRICE_IN_USD_PER_M, GEMINI_PRICE_OUT_USD_PER_M, ANTHROPIC_API_KEY
// and CLAUDE_MODEL_POLISH from the environment; keys are never printed.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jinheng0301/petbridge/pkg/llm"
	"github.com/jinheng0301/petbridge/pkg/llm/claude"
	"github.com/jinheng0301/petbridge/pkg/llm/fake"
	"github.com/jinheng0301/petbridge/pkg/llm/gemini"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "polisheval:", err)
		os.Exit(1)
	}
}

func run() error {
	provider := flag.String("provider", "", "gemini | claude | fake (required)")
	in := flag.String("in", filepath.Join("cmd", "tools", "polisheval", "testdata", "polish_eval.jsonl"), "eval set (JSON lines)")
	langs := flag.String("lang", "", "comma-separated target languages (default all)")
	types := flag.String("types", "", "comma-separated MBTI types (default all 16)")
	n := flag.Int("n", 0, "at most this many cases per language (0 = all)")
	rpm := flag.Int("rpm", 0, "max calls per minute (0 = no pacing; the Gemini free tier needs this)")
	conc := flag.Int("concurrency", 4, "parallel calls")
	out := flag.String("out", "", "CSV path (default tmp/polisheval-<time>.csv)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	p, price, err := buildProvider(ctx, *provider)
	if err != nil {
		return err
	}

	f, err := os.Open(*in)
	if err != nil {
		return err
	}
	cases, err := LoadCases(f)
	f.Close()
	if err != nil {
		return fmt.Errorf("%s: %w", *in, err)
	}
	cases = filterCases(cases, splitList(*langs), *n)
	ts := allTypes
	if l := splitList(*types); len(l) > 0 {
		ts = nil
		for _, t := range l {
			t = strings.ToUpper(t)
			if !contains(allTypes, t) {
				return fmt.Errorf("unknown MBTI type %q", t)
			}
			ts = append(ts, t)
		}
	}
	if len(cases) == 0 {
		return fmt.Errorf("no cases selected")
	}

	calls := 0
	for _, c := range cases {
		if !llm.TooShortToPolish(c.Base) {
			calls += len(ts)
		}
	}
	fmt.Printf("provider %s: %d cases x %d types = %d calls (short cases are not sent)\n", p.Name(), len(cases), len(ts), calls)
	fmt.Println("starting in 3 seconds, Ctrl+C to abort...")
	select {
	case <-time.After(3 * time.Second):
	case <-ctx.Done():
		return nil
	}

	start := time.Now()
	rows := Run(ctx, p, cases, ts, Options{Timeout: 6 * time.Second, Concurrency: *conc, RPM: *rpm})
	sum := Summarize(rows)
	fmt.Printf("\nfinished in %s\n", time.Since(start).Round(time.Second))
	sum.Print(os.Stdout)
	var cost int64
	for model, t := range sum.ModelsByTokens {
		in, outp, ok := price(model)
		if !ok {
			fmt.Printf("no price for model %s; its cost is not counted\n", model)
			continue
		}
		cost += (t[0]*in + t[1]*outp) / 1_000_000
	}
	fmt.Printf("estimated cost: $%.4f\n", float64(cost)/1e6)

	path := *out
	if path == "" {
		path = filepath.Join("tmp", "polisheval-"+time.Now().Format("20060102-150405")+".csv")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	w, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := WriteCSV(w, rows); err != nil {
		w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	fmt.Println("wrote", path)
	return nil
}

type priceFunc func(model string) (in, out int64, ok bool)

func buildProvider(ctx context.Context, name string) (llm.Provider, priceFunc, error) {
	switch name {
	case "fake":
		return fake.New("fake", 0, 0, nil), func(string) (int64, int64, bool) { return 0, 0, true }, nil
	case "claude":
		key := os.Getenv("ANTHROPIC_API_KEY")
		if key == "" {
			return nil, nil, fmt.Errorf("ANTHROPIC_API_KEY is not set")
		}
		model := envOr("CLAUDE_MODEL_POLISH", "claude-haiku-5-5")
		return claude.New(claude.Config{APIKey: key, Model: model, PolishModel: model}), llm.PriceMicros, nil
	case "gemini":
		key, model := os.Getenv("GEMINI_API_KEY"), os.Getenv("GEMINI_MODEL_POLISH")
		if key == "" || model == "" {
			return nil, nil, fmt.Errorf("GEMINI_API_KEY and GEMINI_MODEL_POLISH must be set")
		}
		p, err := gemini.New(ctx, gemini.Config{APIKey: key, Model: model, PolishModel: model})
		if err != nil {
			return nil, nil, err
		}
		return p, geminiPrice(), nil
	}
	return nil, nil, fmt.Errorf("-provider must be gemini, claude or fake")
}

// geminiPrice is 0 on the free tier (GEMINI_FREE_TIER, default true), else
// GEMINI_PRICE_* when both are set.
func geminiPrice() priceFunc {
	if v := os.Getenv("GEMINI_FREE_TIER"); v == "" || v == "true" {
		return func(string) (int64, int64, bool) { return 0, 0, true }
	}
	in, err1 := strconv.ParseFloat(os.Getenv("GEMINI_PRICE_IN_USD_PER_M"), 64)
	out, err2 := strconv.ParseFloat(os.Getenv("GEMINI_PRICE_OUT_USD_PER_M"), 64)
	if err1 != nil || err2 != nil {
		return func(string) (int64, int64, bool) { return 0, 0, false }
	}
	return func(string) (int64, int64, bool) { return int64(in * 1e6), int64(out * 1e6), true }
}

func filterCases(cases []Case, langs []string, n int) []Case {
	var out []Case
	per := map[string]int{}
	for _, c := range cases {
		if len(langs) > 0 && !contains(langs, c.Tgt) {
			continue
		}
		if n > 0 && per[c.Tgt] >= n {
			continue
		}
		per[c.Tgt]++
		out = append(out, c)
	}
	return out
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
