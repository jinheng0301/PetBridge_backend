// Package gemini implements llm.Provider on the Google Gemini API.
package gemini

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"google.golang.org/genai"

	"github.com/jinheng0301/petbridge/pkg/llm"
)

// Config; BaseURL is for tests only.
type Config struct{ APIKey, Model, BaseURL string }

type provider struct {
	client *genai.Client
	model  string
}

func New(ctx context.Context, c Config) (llm.Provider, error) {
	one := int32(1) // a single attempt: the gateway owns retry/breaker logic
	cl, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  c.APIKey,
		Backend: genai.BackendGeminiAPI,
		HTTPOptions: genai.HTTPOptions{
			BaseURL:      c.BaseURL,
			RetryOptions: &genai.HTTPRetryOptions{Attempts: &one},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("gemini: new client: %w", err)
	}
	return &provider{client: cl, model: c.Model}, nil
}

func (p *provider) Name() string { return "gemini" }

func (p *provider) Translate(ctx context.Context, req llm.TranslateRequest) (llm.TranslateResult, error) {
	system, user := llm.TranslatePrompt(req)
	resp, err := p.client.Models.GenerateContent(ctx, p.model,
		[]*genai.Content{genai.NewContentFromText(user, genai.RoleUser)},
		&genai.GenerateContentConfig{
			SystemInstruction:  genai.NewContentFromText(system, genai.RoleUser),
			ResponseMIMEType:   "application/json",
			ResponseJsonSchema: llm.ResultSchema(),
		})
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return llm.TranslateResult{}, cerr
		}
		var ae genai.APIError
		if errors.As(err, &ae) && ae.Code == http.StatusTooManyRequests {
			return llm.TranslateResult{}, fmt.Errorf("gemini: %w", llm.ErrRateLimited)
		}
		var pae *genai.APIError
		if errors.As(err, &pae) && pae.Code == http.StatusTooManyRequests {
			return llm.TranslateResult{}, fmt.Errorf("gemini: %w", llm.ErrRateLimited)
		}
		return llm.TranslateResult{}, fmt.Errorf("gemini: generate: %w", err)
	}
	text, risk, err := llm.ParseResult(resp.Text())
	if err != nil {
		return llm.TranslateResult{}, err
	}
	var u llm.Usage
	if m := resp.UsageMetadata; m != nil {
		u = llm.Usage{InputTokens: int64(m.PromptTokenCount), OutputTokens: int64(m.CandidatesTokenCount) + int64(m.ThoughtsTokenCount)}
	}
	return llm.TranslateResult{Text: text, Risk: risk, Model: p.model, Usage: u}, nil
}
