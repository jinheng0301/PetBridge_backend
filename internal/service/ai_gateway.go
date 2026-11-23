package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/sony/gobreaker/v2"
	"golang.org/x/text/unicode/norm"

	"github.com/jinheng0301/petbridge/pkg/llm"
)

// AICache stores translations and polishes as JSON {"t": text, "r": risk}. Errors (Redis down) are returned; the gateway
// treats a failed Get as a miss and ignores a failed Set.
type AICache interface {
	Get(ctx context.Context, key string) (val string, ok bool, err error)
	Set(ctx context.Context, key, val string, ttl time.Duration) error
}

// CostMeter accumulates the estimated AI spend per UTC day (yyyymmdd).
type CostMeter interface {
	Add(ctx context.Context, day string, micros int64) error
	Total(ctx context.Context, day string) (int64, error)
}

type GatewayConfig struct {
	TranslateTimeout  time.Duration // per provider call; 3s
	PolishTimeout     time.Duration // per provider call; 6s
	MaxConcurrent     int           // in-flight provider chains per process; 16
	DailyBudgetMicros int64         // polish stops at 1x this, translation at 2x
	ZeroCost          map[string]bool
	// Prices are configured per-model prices (model name -> micro-USD per
	// million tokens), consulted before llm's built-in table.
	Prices map[string]llm.Price
}

// AIOutcome is one gateway call's result. It is valid even when err != nil:
// Calls, Usage and CostMicros record calls already paid for (a polish that
// broke a rule). On a cache hit Risk is the stored risk.
type AIOutcome struct {
	Text, Risk, Provider, Model string
	Cached                      bool
	Calls                       int // provider answers received (money spent)
	Usage                       llm.Usage
	CostMicros                  int64
}

type Translator interface {
	Translate(ctx context.Context, req llm.TranslateRequest) (AIOutcome, error)
}

type Polisher interface {
	Polish(ctx context.Context, req llm.PolishRequest) (AIOutcome, error)
}

// ErrPolishRejected matches a *PolishRejectedError: the provider answered but
// broke a rule of backend TRD 10.3.7 (items 2-5).
var ErrPolishRejected = errors.New("ai: polish broke a rule")

type PolishRejectedError struct{ Check string }

func (e *PolishRejectedError) Error() string        { return "ai: polish broke the " + e.Check + " rule" }
func (e *PolishRejectedError) Is(target error) bool { return target == ErrPolishRejected }

const (
	translateCacheTTL = 7 * 24 * time.Hour
	polishCacheTTL    = 7 * 24 * time.Hour

	breakerInterval    = 60 * time.Second
	breakerMinRequests = 20
)

// breakerOpenTimeout is how long a breaker stays open before a half-open probe.
// A var only so tests can shorten it before building a gateway.
var breakerOpenTimeout = 30 * time.Second

type gatewayLink struct {
	p  llm.Provider
	cb *gobreaker.CircuitBreaker[llm.Result] // shared by translate and polish
}

// AIGateway runs the provider chain behind a cache, a per-process concurrency
// limit, one circuit breaker per provider and the daily hard cap.
type AIGateway struct {
	chain  []gatewayLink
	cache  AICache
	meter  CostMeter
	cfg    GatewayConfig
	now    func() time.Time
	log    *slog.Logger
	sem    chan struct{}
	warned sync.Map // model name -> struct{}: unknown-price warning already logged
}

var (
	_ Translator = (*AIGateway)(nil)
	_ Polisher   = (*AIGateway)(nil)
)

// NewAIGateway builds the gateway; providers are tried in order.
func NewAIGateway(providers []llm.Provider, cache AICache, meter CostMeter, cfg GatewayConfig, now func() time.Time, log *slog.Logger) *AIGateway {
	n := cfg.MaxConcurrent
	if n < 1 {
		n = 1
	}
	g := &AIGateway{cache: cache, meter: meter, cfg: cfg, now: now, log: log, sem: make(chan struct{}, n)}
	for _, p := range providers {
		g.chain = append(g.chain, gatewayLink{p: p, cb: gobreaker.NewCircuitBreaker[llm.Result](gobreaker.Settings{
			Name:     p.Name(),
			Interval: breakerInterval,
			Timeout:  breakerOpenTimeout,
			// Excluded calls (see IsExcluded) count neither way: Requests includes
			// them, so they are taken out before the ratio.
			ReadyToTrip: func(c gobreaker.Counts) bool {
				n := c.Requests - c.TotalExclusions
				return n >= breakerMinRequests && c.TotalFailures*2 >= n
			},
			// A caller that gave up says nothing about the provider's health, so
			// it is neither a success nor a failure: in half-open state it neither
			// closes the breaker nor uses up the probe. Every other error,
			// including the per-call TranslateTimeout, is a failure.
			IsExcluded: func(err error) bool { return errors.Is(err, context.Canceled) },
			OnStateChange: func(name string, from, to gobreaker.State) {
				log.Warn("ai breaker state change", "provider", name, "from", from.String(), "to", to.String())
			},
		})})
	}
	return g
}

// TranslateCacheKey is tr:{sha256hex(normalized text)}:{src}:{tgt}:{PromptVersion}.
// Normalization: NFC, trim, collapse runs of Unicode whitespace to one space.
func TranslateCacheKey(req llm.TranslateRequest) string {
	sum := sha256.Sum256([]byte(normalizeForCache(req.Text)))
	return "tr:" + hex.EncodeToString(sum[:]) + ":" + req.SrcLang + ":" + req.TgtLang + ":" + llm.PromptVersion
}

// PolishCacheKey is po:{sha256hex(normalized base)}:{src}:{tgt}:{MBTI}:{tier}:{PolishPromptVersion}.
func PolishCacheKey(req llm.PolishRequest) string {
	sum := sha256.Sum256([]byte(normalizeForCache(req.Text)))
	return "po:" + hex.EncodeToString(sum[:]) + ":" + req.SrcLang + ":" + req.TgtLang + ":" + req.MBTI + ":" + req.Tier + ":" + llm.PolishPromptVersion
}

type cacheEntry struct {
	T string `json:"t"`
	R string `json:"r"`
}

func encodeCacheEntry(res llm.Result) string {
	b, _ := json.Marshal(cacheEntry{T: res.Text, R: res.Risk})
	return string(b)
}

// decodeCacheEntry treats a legacy plain-text value, bad JSON or an empty
// text as a miss.
func decodeCacheEntry(v string) (cacheEntry, bool) {
	var e cacheEntry
	if err := json.Unmarshal([]byte(v), &e); err != nil || e.T == "" {
		return cacheEntry{}, false
	}
	if e.R == "" {
		e.R = llm.RiskNone
	}
	return e, true
}

func normalizeForCache(s string) string {
	// strings.Fields splits on unicode.IsSpace, which drops leading and
	// trailing whitespace too.
	return strings.Join(strings.Fields(norm.NFC.String(s)), " ")
}

// Translate runs the chain. A cache hit is served even over budget (it costs
// nothing); a miss is refused once the day's spend reaches 2x the budget.
func (g *AIGateway) Translate(ctx context.Context, req llm.TranslateRequest) (AIOutcome, error) {
	return g.run(ctx, TranslateCacheKey(req), translateCacheTTL, 2*g.cfg.DailyBudgetMicros, g.cfg.TranslateTimeout,
		func(ctx context.Context, p llm.Provider) (llm.Result, error) { return p.Translate(ctx, req) }, nil)
}

// Polish runs the chain for one polish. A miss is refused once the day's
// spend reaches the budget. An answer that breaks a rule of 10.3.7 is paid
// for, not cached, and not retried on the next provider.
func (g *AIGateway) Polish(ctx context.Context, req llm.PolishRequest) (AIOutcome, error) {
	check := func(res llm.Result) string { return llm.CheckPolish(req.Text, res.Text, req.Tier) }
	return g.run(ctx, PolishCacheKey(req), polishCacheTTL, g.cfg.DailyBudgetMicros, g.cfg.PolishTimeout,
		func(ctx context.Context, p llm.Provider) (llm.Result, error) { return p.Polish(ctx, req) }, check)
}

type providerCall func(ctx context.Context, p llm.Provider) (llm.Result, error)

func (g *AIGateway) run(ctx context.Context, key string, ttl time.Duration, limit int64, timeout time.Duration,
	do providerCall, check func(llm.Result) string) (AIOutcome, error) {
	if v, ok, err := g.cache.Get(ctx, key); err == nil && ok {
		if e, ok := decodeCacheEntry(v); ok {
			return AIOutcome{Text: e.T, Risk: e.R, Cached: true}, nil
		}
	}
	day := g.now().UTC().Format("20060102")
	total, err := g.meter.Total(ctx, day)
	if err != nil {
		total = 0
	}
	if total >= limit {
		return AIOutcome{}, ErrAIBudgetExceeded
	}

	select {
	case g.sem <- struct{}{}:
	case <-ctx.Done():
		return AIOutcome{}, ctx.Err()
	}
	defer func() { <-g.sem }()

	var last error
	for _, l := range g.chain {
		if err := ctx.Err(); err != nil {
			return AIOutcome{}, err
		}
		res, err := g.call(ctx, l, timeout, do)
		if err == nil {
			out := AIOutcome{Text: res.Text, Risk: res.Risk, Provider: l.p.Name(), Model: res.Model,
				Calls: 1, Usage: res.Usage, CostMicros: g.costMicros(l.p.Name(), res)}
			g.addCost(ctx, day, out.CostMicros)
			if check != nil {
				if c := check(res); c != "" {
					g.log.Warn("polish rejected", "check", c, "provider", l.p.Name())
					return out, &PolishRejectedError{Check: c}
				}
			}
			if res.Risk != llm.RiskBlock {
				if err := g.cache.Set(context.WithoutCancel(ctx), key, encodeCacheEntry(res), ttl); err != nil {
					g.log.Warn("ai cache set failed", "err", err)
				}
			}
			return out, nil
		}
		if cerr := ctx.Err(); cerr != nil {
			return AIOutcome{}, cerr
		}
		class := errorClass(err)
		if class == "breaker_open" {
			g.log.Debug("ai provider skipped", "provider", l.p.Name(), "class", class)
		} else {
			g.log.Warn("ai provider failed", "provider", l.p.Name(), "class", class)
		}
		last = err
	}
	if last == nil {
		last = errors.New("no provider configured")
	}
	return AIOutcome{}, fmt.Errorf("%w: %w", ErrAIUnavailable, last)
}

// call runs one provider behind its breaker with the per-call timeout. When
// the caller's ctx has ended the error is marked as context.Canceled so the
// breaker excludes it; the per-call timeout alone (parent still alive)
// stays a DeadlineExceeded failure.
func (g *AIGateway) call(ctx context.Context, l gatewayLink, timeout time.Duration, do providerCall) (llm.Result, error) {
	return l.cb.Execute(func() (llm.Result, error) {
		cctx, cancel := ctx, context.CancelFunc(func() {})
		if timeout > 0 {
			cctx, cancel = context.WithTimeout(ctx, timeout)
		}
		defer cancel()
		res, err := do(cctx, l.p)
		if err != nil && ctx.Err() != nil {
			return res, fmt.Errorf("%w: %w", context.Canceled, err)
		}
		return res, err
	})
}

// meter_ adds the cost; it outlives the caller (the money is spent once the
// provider answered).
func (g *AIGateway) addCost(ctx context.Context, day string, micros int64) {
	if micros <= 0 {
		return
	}
	if err := g.meter.Add(context.WithoutCancel(ctx), day, micros); err != nil {
		g.log.Warn("ai cost add failed", "err", err)
	}
}

func (g *AIGateway) costMicros(provider string, res llm.Result) int64 {
	if g.cfg.ZeroCost[provider] {
		return 0
	}
	in, out, ok := llm.PriceMicros(res.Model)
	if p, set := g.cfg.Prices[res.Model]; set {
		in, out, ok = p.InPerM, p.OutPerM, true
	}
	if !ok {
		if _, seen := g.warned.LoadOrStore(res.Model, struct{}{}); !seen {
			g.log.Warn("ai model has no price; cost counted as 0", "provider", provider, "model", res.Model)
		}
		return 0
	}
	return (res.Usage.InputTokens*in + res.Usage.OutputTokens*out) / 1_000_000
}

func errorClass(err error) string {
	switch {
	case errors.Is(err, gobreaker.ErrOpenState), errors.Is(err, gobreaker.ErrTooManyRequests):
		return "breaker_open"
	case errors.Is(err, llm.ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, llm.ErrBadOutput):
		return "bad_output"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "error"
	}
}
