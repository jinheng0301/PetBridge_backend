// Package llm defines the provider-neutral interface the AI gateway builds on,
// plus the translation prompt, output parser and price table. Provider
// implementations live in sub-packages (gemini, claude, fake); no SDK type is
// exposed outside them.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// PromptVersion must be bumped whenever any translation prompt text changes
// (it is part of the translation cache key).
const PromptVersion = "t2"

type TranslateRequest struct{ Text, SrcLang, TgtLang string }

type Usage struct{ InputTokens, OutputTokens int64 }

type TranslateResult struct {
	Text, Risk, Model string
	Usage             Usage
}

type Provider interface {
	// Name is "gemini" | "claude" | "fake-primary" | "fake-secondary".
	Name() string
	Translate(ctx context.Context, req TranslateRequest) (TranslateResult, error)
}

var (
	ErrRateLimited = errors.New("llm: rate limited")      // HTTP 429
	ErrBadOutput   = errors.New("llm: unparsable output") // not the agreed JSON, or empty text
)

// Risk values.
const (
	RiskNone   = "none"
	RiskReview = "review"
	RiskBlock  = "block"
)

// ParseResult parses {"text","risk"}. A missing or unknown risk becomes "none";
// an empty (after trimming) text returns ErrBadOutput.
func ParseResult(raw string) (text, risk string, err error) {
	var v struct {
		Text string `json:"text"`
		Risk string `json:"risk"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &v); err != nil {
		return "", "", ErrBadOutput
	}
	text = strings.TrimSpace(v.Text)
	if text == "" {
		return "", "", ErrBadOutput
	}
	switch v.Risk {
	case RiskReview, RiskBlock:
		risk = v.Risk
	default:
		risk = RiskNone
	}
	return text, risk, nil
}

// ResultSchema is the JSON schema both providers are asked to follow.
func ResultSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string"},
			"risk": map[string]any{"type": "string", "enum": []string{RiskNone, RiskReview, RiskBlock}},
		},
		"required":             []string{"text", "risk"},
		"additionalProperties": false,
	}
}
