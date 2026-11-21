// Package bootstrap builds shared dependencies for the api and worker processes.
package bootstrap

import (
	"context"
	"math/rand/v2"

	"github.com/jinheng0301/petbridge/internal/config"
	"github.com/jinheng0301/petbridge/pkg/llm"
	"github.com/jinheng0301/petbridge/pkg/llm/claude"
	"github.com/jinheng0301/petbridge/pkg/llm/fake"
	"github.com/jinheng0301/petbridge/pkg/llm/gemini"
)

// Providers builds the ordered provider chain (primary first). It never logs
// or returns API keys.
func Providers(ctx context.Context, c *config.Config) ([]llm.Provider, error) {
	if c.AIProvider != "live" {
		return []llm.Provider{
			fake.New("fake-primary", c.FakeAILatency, c.FakeAIFailRate, rand.Float64),
			fake.New("fake-secondary", c.FakeAILatency, c.FakeAISecondaryFailRate, rand.Float64),
		}, nil
	}
	var gem, cla llm.Provider
	if c.GeminiAPIKey != "" {
		g, err := gemini.New(ctx, gemini.Config{APIKey: c.GeminiAPIKey, Model: c.GeminiModelTranslate})
		if err != nil {
			return nil, err
		}
		gem = g
	}
	if c.AnthropicAPIKey != "" {
		cla = claude.New(claude.Config{APIKey: c.AnthropicAPIKey, Model: c.ClaudeModelTranslate})
	}
	order := []llm.Provider{gem, cla}
	if c.AIPrimary == "claude" {
		order = []llm.Provider{cla, gem}
	}
	var out []llm.Provider
	for _, p := range order {
		if p != nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// ZeroCost lists providers whose usage cost is recorded as 0: the fake
// providers, and Gemini while on the free tier.
func ZeroCost(c *config.Config) map[string]bool {
	z := map[string]bool{}
	if c.AIProvider != "live" {
		z["fake-primary"], z["fake-secondary"] = true, true
	} else if c.GeminiFreeTier {
		z["gemini"] = true
	}
	return z
}
