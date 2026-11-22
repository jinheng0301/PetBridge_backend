package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jinheng0301/petbridge/pkg/llm"
)

// Case is one line of the eval set.
type Case struct {
	ID       string `json:"id"`
	Tgt      string `json:"tgt"`
	Src      string `json:"src"`
	Category string `json:"category"`
	Source   string `json:"source"`
	Base     string `json:"base"`
}

// Row is one case polished for one MBTI type.
type Row struct {
	Case
	MBTI, Polished, Status string // Status: pass | short | violation:<check> | error:<class>
	InTokens, OutTokens    int64
	Model                  string
}

type Options struct {
	Timeout     time.Duration // per call; 6s like the gateway
	Concurrency int           // default 4
	RPM         int           // 0 = no pacing
}

// Categories from backend TRD 10.3.9.
var validCategory = map[string]bool{
	"numbers_time": true, "request": true, "refusal": true, "question": true, "joke": true,
	"negative": true, "names_links": true, "short": true, "injection": true,
}

var allTypes = func() []string {
	var out []string
	for _, a := range "EI" {
		for _, b := range "NS" {
			for _, c := range "TF" {
				for _, d := range "JP" {
					out = append(out, string([]rune{a, b, c, d}))
				}
			}
		}
	}
	return out
}()

// LoadCases reads JSON lines; blank lines are skipped.
func LoadCases(r io.Reader) ([]Case, error) {
	var out []Case
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var c Case
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		if c.ID == "" || c.Tgt == "" || c.Src == "" || c.Base == "" {
			return nil, fmt.Errorf("line %d: id, tgt, src and base are required", n)
		}
		out = append(out, c)
	}
	return out, sc.Err()
}

// Run polishes every case for every type. Rows come back in case × type order.
func Run(ctx context.Context, p llm.Provider, cases []Case, types []string, opt Options) []Row {
	if opt.Concurrency < 1 {
		opt.Concurrency = 4
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 6 * time.Second
	}
	rows := make([]Row, 0, len(cases)*len(types))
	for _, c := range cases {
		for _, t := range types {
			rows = append(rows, Row{Case: c, MBTI: t})
		}
	}
	var tick <-chan time.Time
	if opt.RPM > 0 {
		tk := time.NewTicker(time.Minute / time.Duration(opt.RPM))
		defer tk.Stop()
		tick = tk.C
	}
	sem := make(chan struct{}, opt.Concurrency)
	var wg sync.WaitGroup
	for i := range rows {
		r := &rows[i]
		if llm.TooShortToPolish(r.Base) {
			r.Status, r.Polished = "short", r.Base
			continue
		}
		if tick != nil {
			select {
			case <-tick:
			case <-ctx.Done():
			}
		}
		if ctx.Err() != nil {
			r.Status = "error:cancelled"
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			polishOne(ctx, p, r, opt.Timeout)
		}()
	}
	wg.Wait()
	return rows
}

func polishOne(ctx context.Context, p llm.Provider, r *Row, timeout time.Duration) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	src := r.Source
	if r.Src == r.Tgt {
		src = ""
	}
	res, err := p.Polish(cctx, llm.PolishRequest{Text: r.Base, Source: src, SrcLang: r.Src, TgtLang: r.Tgt, MBTI: r.MBTI, Tier: llm.TierLight})
	if err != nil {
		r.Status = "error:" + errClass(err)
		return
	}
	r.Polished, r.Model = res.Text, res.Model
	r.InTokens, r.OutTokens = res.Usage.InputTokens, res.Usage.OutputTokens
	if check := llm.CheckPolish(r.Base, res.Text, llm.TierLight); check != "" {
		r.Status = "violation:" + check
		return
	}
	r.Status = "pass"
}

func errClass(err error) string {
	switch {
	case errors.Is(err, llm.ErrBadOutput):
		return "bad_output"
	case errors.Is(err, llm.ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	return "error"
}

type LangSummary struct {
	Total, Short, Pass int
	Fails              map[string]int // status -> count, for everything but pass/short
}

// PassRate is pass / (total - short); 0 when nothing was polished.
func (l LangSummary) PassRate() float64 {
	n := l.Total - l.Short
	if n == 0 {
		return 0
	}
	return float64(l.Pass) / float64(n)
}

type Summary struct {
	Langs          map[string]*LangSummary
	InTokens       int64
	OutTokens      int64
	ModelsByTokens map[string][2]int64 // model -> {in, out}
}

func Summarize(rows []Row) Summary {
	s := Summary{Langs: map[string]*LangSummary{}, ModelsByTokens: map[string][2]int64{}}
	for _, r := range rows {
		l := s.Langs[r.Tgt]
		if l == nil {
			l = &LangSummary{Fails: map[string]int{}}
			s.Langs[r.Tgt] = l
		}
		l.Total++
		switch r.Status {
		case "pass":
			l.Pass++
		case "short":
			l.Short++
		default:
			l.Fails[r.Status]++
		}
		s.InTokens += r.InTokens
		s.OutTokens += r.OutTokens
		if r.Model != "" {
			t := s.ModelsByTokens[r.Model]
			s.ModelsByTokens[r.Model] = [2]int64{t[0] + r.InTokens, t[1] + r.OutTokens}
		}
	}
	return s
}

// Print writes the per-language summary.
func (s Summary) Print(w io.Writer) {
	langs := make([]string, 0, len(s.Langs))
	for l := range s.Langs {
		langs = append(langs, l)
	}
	sort.Strings(langs)
	for _, name := range langs {
		l := s.Langs[name]
		fmt.Fprintf(w, "%-8s pass %5.1f%%  (%d pass / %d polished, %d short)\n",
			name, 100*l.PassRate(), l.Pass, l.Total-l.Short, l.Short)
		keys := make([]string, 0, len(l.Fails))
		for k := range l.Fails {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "         %-20s %d\n", k, l.Fails[k])
		}
	}
	fmt.Fprintf(w, "tokens: in %d, out %d\n", s.InTokens, s.OutTokens)
}

// WriteCSV writes a UTF-8 (with BOM) sheet for native-speaker scoring.
func WriteCSV(w io.Writer, rows []Row) error {
	if _, err := w.Write([]byte("\xEF\xBB\xBF")); err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"id", "tgt", "category", "mbti", "source", "base", "polished", "status", "原意保持", "自然度", "风格贴合"}); err != nil {
		return err
	}
	for _, r := range rows {
		if err := cw.Write([]string{r.ID, r.Tgt, r.Category, r.MBTI, r.Source, r.Base, r.Polished, r.Status, "", "", ""}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
