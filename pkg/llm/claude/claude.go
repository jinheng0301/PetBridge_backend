// Package claude implements llm.Provider on the Anthropic Messages API.
package claude

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	anthropic "github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/jinheng0301/petbridge/pkg/llm"
)

// Config; BaseURL is for tests only.
type Config struct{ APIKey, Model, BaseURL string }

type provider struct {
	client anthropic.Client
	model  string
}

func New(c Config) llm.Provider {
	opts := []option.RequestOption{
		option.WithAPIKey(c.APIKey),
		option.WithMaxRetries(0), // the gateway owns retry/breaker logic
	}
	if c.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(c.BaseURL))
	}
	return &provider{client: anthropic.NewClient(opts...), model: c.Model}
}

func (p *provider) Name() string { return "claude" }

func (p *provider) Translate(ctx context.Context, req llm.TranslateRequest) (llm.TranslateResult, error) {
	system, user := llm.TranslatePrompt(req)
	msg, err := p.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     p.model,
		MaxTokens: 2048,
		System:    []anthropic.TextBlockParam{{Text: system}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(user))},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffortLow,
			Format: anthropic.JSONOutputFormatParam{Schema: llm.ResultSchema()},
		},
	})
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return llm.TranslateResult{}, cerr
		}
		var ae *anthropic.Error
		if errors.As(err, &ae) && ae.StatusCode == http.StatusTooManyRequests {
			return llm.TranslateResult{}, fmt.Errorf("claude: %w", llm.ErrRateLimited)
		}
		return llm.TranslateResult{}, fmt.Errorf("claude: messages: %w", err)
	}
	var raw string
	for _, b := range msg.Content {
		if b.Type == "text" {
			raw += b.Text
		}
	}
	text, risk, err := llm.ParseResult(raw)
	if err != nil {
		return llm.TranslateResult{}, err
	}
	return llm.TranslateResult{
		Text: text, Risk: risk, Model: p.model,
		Usage: llm.Usage{InputTokens: msg.Usage.InputTokens, OutputTokens: msg.Usage.OutputTokens},
	}, nil
}
