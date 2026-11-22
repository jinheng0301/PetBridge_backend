package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// AICache stores translations. Errors (Redis down) are returned; the gateway
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
	MaxConcurrent     int           // in-flight provider chains per process; 16
	DailyBudgetMicros int64         // translation stops at 2x this
	ZeroCost          map[string]bool
	// Prices are configured per-model prices (model name -> micro-USD per
	// million tokens), consulted before llm's built-in table.
	Prices map[string]llm.Price
}

// Translator returns the translation and the provider's risk verdict ("none"
// on a cache hit).
type Translator interface {
	Translate(ctx context.Context, req llm.TranslateRequest) (text, risk string, err error)
}

const (
	translateCacheTTL = 7 * 24 * time.Hour

	breakerInterval    = 60 * time.Second
	breakerMinRequests = 20
)

// breakerOpenTimeout is how long a breaker stays open before a half-open probe.
// A var only so tests can shorten it before building a gateway.
var breakerOpenTimeout = 30 * time.Second

type gatewayLink struct {
	p  llm.Provider
	cb *gobreaker.CircuitBreaker[llm.TranslateResult]
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

var _ Translator = (*AIGateway)(nil)

// NewAIGateway builds the gateway; providers are tried in order.
func NewAIGateway(providers []llm.Provider, cache AICache, meter CostMeter, cfg GatewayConfig, now func() time.Time, log *slog.Logger) *AIGateway {
	n := cfg.MaxConcurrent
	if n < 1 {
		n = 1
	}
	g := &AIGateway{cache: cache, meter: meter, cfg: cfg, now: now, log: log, sem: make(chan struct{}, n)}
	for _, p := range providers {
		g.chain = append(g.chain, gatewayLink{p: p, cb: gobreaker.NewCircuitBreaker[llm.TranslateResult](gobreaker.Settings{
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

func normalizeForCache(s string) string {
	// strings.Fields splits on unicode.IsSpace, which drops leading and
	// trailing whitespace too.
	return strings.Join(strings.Fields(norm.NFC.String(s)), " ")
}

func (g *AIGateway) Translate(ctx context.Context, req llm.TranslateRequest) (string, string, error) {
	day := g.now().UTC().Format("20060102")
	total, err := g.meter.Total(ctx, day)
	if err != nil {
		total = 0
	}
	if total >= 2*g.cfg.DailyBudgetMicros {
		return "", "", ErrAIBudgetExceeded
	}

	key := TranslateCacheKey(req)
	if v, ok, err := g.cache.Get(ctx, key); err == nil && ok {
		return v, llm.RiskNone, nil
	}

	select {
	case g.sem <- struct{}{}:
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
	defer func() { <-g.sem }()

	var last error
	for _, l := range g.chain {
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
		res, err := g.call(ctx, l, req)
		if err == nil {
			g.record(ctx, key, day, l.p.Name(), res)
			return res.Text, res.Risk, nil
		}
		if cerr := ctx.Err(); cerr != nil {
			return "", "", cerr
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
	return "", "", fmt.Errorf("%w: %w", ErrAIUnavailable, last)
}

// call runs one provider behind its breaker with the per-call timeout. When
// the caller's ctx has ended the error is marked as context.Canceled so the
// breaker excludes it; the per-call timeout alone (parent still alive)
// stays a DeadlineExceeded failure.
func (g *AIGateway) call(ctx context.Context, l gatewayLink, req llm.TranslateRequest) (llm.TranslateResult, error) {
	return l.cb.Execute(func() (llm.TranslateResult, error) {
		cctx, cancel := ctx, context.CancelFunc(func() {})
		if g.cfg.TranslateTimeout > 0 {
			cctx, cancel = context.WithTimeout(ctx, g.cfg.TranslateTimeout)
		}
		defer cancel()
		res, err := l.p.Translate(cctx, req)
		if err != nil && ctx.Err() != nil {
			return res, fmt.Errorf("%w: %w", context.Canceled, err)
		}
		return res, err
	})
}

// record caches the translation and meters its cost. Both outlive the caller:
// the money is spent once the provider answered.
func (g *AIGateway) record(ctx context.Context, key, day, provider string, res llm.TranslateResult) {
	bg := context.WithoutCancel(ctx)
	if err := g.cache.Set(bg, key, res.Text, translateCacheTTL); err != nil {
		g.log.Warn("ai cache set failed", "err", err)
	}
	micros := g.costMicros(provider, res)
	if micros <= 0 {
		return
	}
	if err := g.meter.Add(bg, day, micros); err != nil {
		g.log.Warn("ai cost add failed", "err", err)
	}
}

func (g *AIGateway) costMicros(provider string, res llm.TranslateResult) int64 {
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
