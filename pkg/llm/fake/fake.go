// Package fake is a deterministic in-process provider for tests and local dev.
package fake

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/jinheng0301/petbridge/pkg/llm"
)

type provider struct {
	name     string
	latency  time.Duration
	failRate float64
	rnd      func() float64
}

// New returns a provider that answers "[tgt] text" with Model "fake" after
// latency, failing with probability failRate. rnd (nil = math/rand/v2) draws
// values in [0,1); a draw < failRate fails.
func New(name string, latency time.Duration, failRate float64, rnd func() float64) llm.Provider {
	if rnd == nil {
		rnd = rand.Float64
	}
	return &provider{name: name, latency: latency, failRate: failRate, rnd: rnd}
}

func (p *provider) Name() string { return p.name }

func (p *provider) Translate(ctx context.Context, req llm.TranslateRequest) (llm.TranslateResult, error) {
	if p.latency > 0 {
		t := time.NewTimer(p.latency)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return llm.TranslateResult{}, ctx.Err()
		case <-t.C:
		}
	}
	if err := ctx.Err(); err != nil {
		return llm.TranslateResult{}, err
	}
	if p.failRate > 0 && p.rnd() < p.failRate {
		return llm.TranslateResult{}, errors.New("fake: injected failure")
	}
	return llm.TranslateResult{
		Text:  "[" + req.TgtLang + "] " + req.Text,
		Risk:  llm.RiskNone,
		Model: "fake",
		Usage: llm.Usage{InputTokens: int64(len(req.Text)), OutputTokens: int64(len(req.Text))},
	}, nil
}
