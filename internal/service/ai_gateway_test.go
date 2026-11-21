package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/jinheng0301/petbridge/pkg/llm"
)

// gwProvider is a scripted llm.Provider for the gateway tests.
type gwProvider struct {
	name  string
	model string
	usage llm.Usage
	err   error         // returned when non-nil
	sleep time.Duration // honours ctx while sleeping

	calls    atomic.Int64
	inFlight atomic.Int64
	maxSeen  atomic.Int64
}

func (p *gwProvider) Name() string { return p.name }

func (p *gwProvider) Translate(ctx context.Context, req llm.TranslateRequest) (llm.TranslateResult, error) {
	p.calls.Add(1)
	n := p.inFlight.Add(1)
	defer p.inFlight.Add(-1)
	for {
		m := p.maxSeen.Load()
		if n <= m || p.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	if p.sleep > 0 {
		select {
		case <-time.After(p.sleep):
		case <-ctx.Done():
			return llm.TranslateResult{}, ctx.Err()
		}
	}
	if p.err != nil {
		return llm.TranslateResult{}, p.err
	}
	return llm.TranslateResult{Text: p.name + ":" + req.Text, Risk: llm.RiskReview, Model: p.model, Usage: p.usage}, nil
}

type gwCache struct {
	mu   sync.Mutex
	data map[string]string
	sets int
}

func newGWCache() *gwCache { return &gwCache{data: map[string]string{}} }

func (c *gwCache) Get(_ context.Context, key string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.data[key]
	return v, ok, nil
}

func (c *gwCache) Set(_ context.Context, key, val string, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[key] = val
	c.sets++
	return nil
}

type gwMeter struct {
	mu   sync.Mutex
	data map[string]int64
	adds int
}

func newGWMeter() *gwMeter { return &gwMeter{data: map[string]int64{}} }

func (m *gwMeter) Add(_ context.Context, day string, micros int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[day] += micros
	m.adds++
	return nil
}

func (m *gwMeter) Total(_ context.Context, day string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data[day], nil
}

var gwNow = time.Date(2026, 11, 22, 3, 0, 0, 0, time.FixedZone("UTC+8", 8*3600))

const gwDay = "20261121" // gwNow in UTC is 2026-11-21 19:00; the local date is the 22nd

func gwConfig() GatewayConfig {
	return GatewayConfig{TranslateTimeout: 3 * time.Second, MaxConcurrent: 16, DailyBudgetMicros: 5_000_000}
}

func newTestGateway(cfg GatewayConfig, cache AICache, meter CostMeter, log *slog.Logger, ps ...llm.Provider) *AIGateway {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return NewAIGateway(ps, cache, meter, cfg, func() time.Time { return gwNow }, log)
}

func gwReq(text string) llm.TranslateRequest {
	return llm.TranslateRequest{Text: text, SrcLang: "zh", TgtLang: "en"}
}

func TestGateway_PrimaryThenSecondary(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	p := &gwProvider{name: "fake-primary", err: errors.New("boom")}
	s := &gwProvider{name: "fake-secondary"}
	g := newTestGateway(gwConfig(), newGWCache(), newGWMeter(), log, p, s)
	text, risk, err := g.Translate(context.Background(), gwReq("secret words"))
	if err != nil {
		t.Fatal(err)
	}
	if text != "fake-secondary:secret words" || risk != llm.RiskReview {
		t.Fatalf("got %q / %q", text, risk)
	}
	if p.calls.Load() != 1 || s.calls.Load() != 1 {
		t.Fatalf("calls = %d / %d, want 1 / 1", p.calls.Load(), s.calls.Load())
	}
	if !strings.Contains(logs.String(), "fake-primary") {
		t.Fatalf("provider failure not logged: %s", logs.String())
	}
	if strings.Contains(logs.String(), "secret") {
		t.Fatalf("log leaks message text: %s", logs.String())
	}
}

func TestGateway_FallsBackOnBadOutput(t *testing.T) {
	p := &gwProvider{name: "fake-primary", err: llm.ErrBadOutput}
	s := &gwProvider{name: "fake-secondary"}
	g := newTestGateway(gwConfig(), newGWCache(), newGWMeter(), nil, p, s)
	text, _, err := g.Translate(context.Background(), gwReq("hi"))
	if err != nil || text != "fake-secondary:hi" {
		t.Fatalf("got %q, %v", text, err)
	}
}

func TestGateway_AllFail(t *testing.T) {
	p := &gwProvider{name: "fake-primary", err: llm.ErrRateLimited}
	s := &gwProvider{name: "fake-secondary", err: llm.ErrBadOutput}
	g := newTestGateway(gwConfig(), newGWCache(), newGWMeter(), nil, p, s)
	_, _, err := g.Translate(context.Background(), gwReq("hi"))
	if !errors.Is(err, ErrAIUnavailable) {
		t.Fatalf("err = %v, want ErrAIUnavailable", err)
	}
	if !errors.Is(err, llm.ErrBadOutput) {
		t.Fatalf("err = %v, want it to wrap the last error", err)
	}
}

func TestGateway_CacheHit(t *testing.T) {
	cache, meter := newGWCache(), newGWMeter()
	p := &gwProvider{name: "claude", model: "claude-haiku-5-5", usage: llm.Usage{InputTokens: 1000, OutputTokens: 1000}}
	g := newTestGateway(gwConfig(), cache, meter, nil, p)
	text1, _, err := g.Translate(context.Background(), gwReq("你好  世界"))
	if err != nil {
		t.Fatal(err)
	}
	adds := meter.adds
	text2, risk, err := g.Translate(context.Background(), gwReq(" \t你好 \n 世界　"))
	if err != nil {
		t.Fatal(err)
	}
	if p.calls.Load() != 1 {
		t.Fatalf("provider calls = %d, want 1", p.calls.Load())
	}
	if meter.adds != adds {
		t.Fatalf("cache hit added cost")
	}
	if text2 != text1 || risk != llm.RiskNone {
		t.Fatalf("hit = %q / %q, want %q / none", text2, risk, text1)
	}
}

func TestTranslateCacheKey(t *testing.T) {
	a := TranslateCacheKey(gwReq("a  b"))
	if !strings.HasPrefix(a, "tr:") || !strings.HasSuffix(a, ":zh:en:"+llm.PromptVersion) {
		t.Fatalf("key = %q", a)
	}
	if len(a) != len("tr:")+64+len(":zh:en:")+len(llm.PromptVersion) {
		t.Fatalf("key = %q, want a sha256 hex digest", a)
	}
	if b := TranslateCacheKey(gwReq(" a \tb ")); b != a {
		t.Fatalf("whitespace variants differ: %q vs %q", a, b)
	}
	// NFC: "é" precomposed vs e + combining acute.
	if TranslateCacheKey(gwReq("café")) != TranslateCacheKey(gwReq("café")) {
		t.Fatal("NFC variants differ")
	}
	if TranslateCacheKey(gwReq("a b")) == TranslateCacheKey(llm.TranslateRequest{Text: "a b", SrcLang: "zh", TgtLang: "ms"}) {
		t.Fatal("target language not in key")
	}
}

func TestGateway_BreakerOpensAndSkips(t *testing.T) {
	p := &gwProvider{name: "fake-primary", err: errors.New("down")}
	s := &gwProvider{name: "fake-secondary"}
	g := newTestGateway(gwConfig(), newGWCache(), newGWMeter(), nil, p, s)
	for i := 0; i < 20; i++ {
		if _, _, err := g.Translate(context.Background(), gwReq(fmt.Sprintf("msg %d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if p.calls.Load() != 20 {
		t.Fatalf("primary calls = %d, want 20", p.calls.Load())
	}
	text, _, err := g.Translate(context.Background(), gwReq("msg 21"))
	if err != nil || text != "fake-secondary:msg 21" {
		t.Fatalf("got %q, %v", text, err)
	}
	if p.calls.Load() != 20 {
		t.Fatalf("primary called while breaker open: %d", p.calls.Load())
	}
}

func TestGateway_CallerCancelNotCountedAsFailure(t *testing.T) {
	p := &gwProvider{name: "fake-primary", sleep: 5 * time.Second}
	s := &gwProvider{name: "fake-secondary"}
	g := newTestGateway(gwConfig(), newGWCache(), newGWMeter(), nil, p, s)
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		_, _, err := g.Translate(ctx, gwReq(fmt.Sprintf("msg %d", i)))
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the caller's ctx error", err)
		}
	}
	if s.calls.Load() != 0 {
		t.Fatalf("secondary tried after caller gave up: %d", s.calls.Load())
	}
	// The breaker must still be closed: the next call reaches the primary.
	p.sleep = 0
	if _, _, err := g.Translate(context.Background(), gwReq("after")); err != nil {
		t.Fatal(err)
	}
	if p.calls.Load() != 21 {
		t.Fatalf("primary calls = %d, want 21 (breaker opened on caller cancels)", p.calls.Load())
	}
	// The cancels were excluded, not counted as successes: 20 real failures
	// after them still trip the breaker.
	cb := g.chain[0].cb
	if c := cb.Counts(); c.TotalExclusions != 20 || c.TotalSuccesses != 1 {
		t.Fatalf("counts = %+v, want 20 exclusions and 1 success", c)
	}
}

func TestGateway_CallerCancelDuringHalfOpenKeepsBreakerOpen(t *testing.T) {
	old := breakerOpenTimeout
	breakerOpenTimeout = 50 * time.Millisecond
	t.Cleanup(func() { breakerOpenTimeout = old })

	p := &gwProvider{name: "fake-primary", err: errors.New("down")}
	s := &gwProvider{name: "fake-secondary"}
	g := newTestGateway(gwConfig(), newGWCache(), newGWMeter(), nil, p, s)
	cb := g.chain[0].cb
	for i := 0; i < 20; i++ {
		if _, _, err := g.Translate(context.Background(), gwReq(fmt.Sprintf("msg %d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if cb.State() != gobreaker.StateOpen {
		t.Fatalf("state = %v, want open", cb.State())
	}
	time.Sleep(80 * time.Millisecond)
	if cb.State() != gobreaker.StateHalfOpen {
		t.Fatalf("state = %v, want half-open", cb.State())
	}

	// The probe's caller disconnects: the breaker must not close.
	p.err, p.sleep = nil, 5*time.Second
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)
	if _, _, err := g.Translate(ctx, gwReq("probe")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if cb.State() != gobreaker.StateHalfOpen {
		t.Fatalf("state after cancelled probe = %v, want still half-open", cb.State())
	}
	calls := p.calls.Load()

	// The next real probe fails: the breaker re-opens and the primary is skipped.
	p.err, p.sleep = errors.New("still down"), 0
	if _, _, err := g.Translate(context.Background(), gwReq("probe 2")); err != nil {
		t.Fatal(err)
	}
	if p.calls.Load() != calls+1 {
		t.Fatalf("half-open probe not sent to primary: calls %d -> %d", calls, p.calls.Load())
	}
	if cb.State() != gobreaker.StateOpen {
		t.Fatalf("state after failed probe = %v, want open", cb.State())
	}
	if _, _, err := g.Translate(context.Background(), gwReq("after")); err != nil {
		t.Fatal(err)
	}
	if p.calls.Load() != calls+1 {
		t.Fatal("primary called while breaker re-opened")
	}
}

func TestGateway_PerCallTimeoutCountsAsFailure(t *testing.T) {
	cfg := gwConfig()
	cfg.TranslateTimeout = time.Millisecond
	p := &gwProvider{name: "fake-primary", sleep: time.Second}
	s := &gwProvider{name: "fake-secondary"}
	g := newTestGateway(cfg, newGWCache(), newGWMeter(), nil, p, s)
	if _, _, err := g.Translate(context.Background(), gwReq("hi")); err != nil {
		t.Fatal(err)
	}
	if c := g.chain[0].cb.Counts(); c.TotalFailures != 1 || c.TotalExclusions != 0 {
		t.Fatalf("counts = %+v, want the timeout counted as 1 failure", c)
	}
}

func TestGateway_Timeout(t *testing.T) {
	cfg := gwConfig()
	cfg.TranslateTimeout = 50 * time.Millisecond
	p := &gwProvider{name: "fake-primary", sleep: 5 * time.Second}
	s := &gwProvider{name: "fake-secondary"}
	g := newTestGateway(cfg, newGWCache(), newGWMeter(), nil, p, s)
	start := time.Now()
	text, _, err := g.Translate(context.Background(), gwReq("hi"))
	if err != nil || text != "fake-secondary:hi" {
		t.Fatalf("got %q, %v", text, err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("fallback took %v, want ~50ms", d)
	}
}

func TestGateway_CostAndHardCap(t *testing.T) {
	meter := newGWMeter()
	haiku := &gwProvider{name: "claude", model: "claude-haiku-5-5", usage: llm.Usage{InputTokens: 1_000_000}}
	g := newTestGateway(gwConfig(), newGWCache(), meter, nil, haiku)
	if _, _, err := g.Translate(context.Background(), gwReq("hi")); err != nil {
		t.Fatal(err)
	}
	if got := meter.data[gwDay]; got != 100000 {
		t.Fatalf("cost = %d, want 100000 (day keys %v)", got, meter.data)
	}

	free := newGWMeter()
	cfg := gwConfig()
	cfg.ZeroCost = map[string]bool{"gemini": true}
	gem := &gwProvider{name: "gemini", model: "claude-haiku-5-5", usage: llm.Usage{InputTokens: 1_000_000}}
	g = newTestGateway(cfg, newGWCache(), free, nil, gem)
	if _, _, err := g.Translate(context.Background(), gwReq("hi")); err != nil {
		t.Fatal(err)
	}
	if got := free.data[gwDay]; got != 0 {
		t.Fatalf("zero-cost provider cost = %d", got)
	}

	unknown := newGWMeter()
	odd := &gwProvider{name: "claude", model: "some-new-model", usage: llm.Usage{InputTokens: 1_000_000}}
	g = newTestGateway(gwConfig(), newGWCache(), unknown, nil, odd)
	if _, _, err := g.Translate(context.Background(), gwReq("hi")); err != nil {
		t.Fatal(err)
	}
	if got := unknown.data[gwDay]; got != 0 {
		t.Fatalf("unknown model cost = %d", got)
	}

	capped := newGWMeter()
	capped.data[gwDay] = 200
	cfg = gwConfig()
	cfg.DailyBudgetMicros = 100
	p := &gwProvider{name: "claude", model: "claude-haiku-5-5"}
	g = newTestGateway(cfg, newGWCache(), capped, nil, p)
	if _, _, err := g.Translate(context.Background(), gwReq("hi")); !errors.Is(err, ErrAIBudgetExceeded) {
		t.Fatalf("err = %v, want ErrAIBudgetExceeded", err)
	}
	if p.calls.Load() != 0 {
		t.Fatalf("provider called past the hard cap")
	}
	capped.data[gwDay] = 199
	if _, _, err := g.Translate(context.Background(), gwReq("hi")); err != nil {
		t.Fatalf("below 2x budget: %v", err)
	}
}

func TestGateway_ConcurrencyLimit(t *testing.T) {
	cfg := gwConfig()
	cfg.MaxConcurrent = 2
	p := &gwProvider{name: "fake-primary", sleep: 100 * time.Millisecond}
	g := newTestGateway(cfg, newGWCache(), newGWMeter(), nil, p)
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := g.Translate(context.Background(), gwReq(fmt.Sprintf("slow %d", i)))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := p.maxSeen.Load(); got != 2 {
		t.Fatalf("max concurrency = %d, want 2", got)
	}
}

func TestGateway_SemaphoreWaitHonoursCtx(t *testing.T) {
	cfg := gwConfig()
	cfg.MaxConcurrent = 1
	p := &gwProvider{name: "fake-primary", sleep: 300 * time.Millisecond}
	g := newTestGateway(cfg, newGWCache(), newGWMeter(), nil, p)
	go g.Translate(context.Background(), gwReq("holder")) //nolint:errcheck
	time.Sleep(30 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := g.Translate(ctx, gwReq("waiter")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want ctx error", err)
	}
}
