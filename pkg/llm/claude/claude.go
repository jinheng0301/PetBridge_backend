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
type Config struct{ APIKey, Model, PolishModel, BaseURL string }

type provider struct {
	client             anthropic.Client
	model, polishModel string
}

func New(c Config) llm.Provider {
	opts := []option.RequestOption{
		option.WithAPIKey(c.APIKey),
		option.WithMaxRetries(0), // the gateway owns retry/breaker logic
	}
	if c.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(c.BaseURL))
	}
	return &provider{client: anthropic.NewClient(opts...), model: c.Model, polishModel: c.PolishModel}
}

func (p *provider) Name() string { return "claude" }

func (p *provider) Translate(ctx context.Context, req llm.TranslateRequest) (llm.Result, error) {
	system, user := llm.TranslatePrompt(req)
	return p.generate(ctx, p.model, system, user)
}

func (p *provider) Polish(ctx context.Context, req llm.PolishRequest) (llm.Result, error) {
	system, user := llm.PolishPrompt(req)
	return p.generate(ctx, p.polishModel, system, user)
}

func (p *provider) generate(ctx context.Context, model, system, user string) (llm.Result, error) {
	msg, err := p.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     model,
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
			return llm.Result{}, cerr
		}
		var ae *anthropic.Error
		if errors.As(err, &ae) && ae.StatusCode == http.StatusTooManyRequests {
			return llm.Result{}, fmt.Errorf("claude: %w", llm.ErrRateLimited)
		}
		return llm.Result{}, fmt.Errorf("claude: messages: %w", err)
	}
	var raw string
	for _, b := range msg.Content {
		if b.Type == "text" {
			raw += b.Text
		}
	}
	text, risk, err := llm.ParseResult(raw)
	if err != nil {
		return llm.Result{}, err
	}
	return llm.Result{
		Text: text, Risk: risk, Model: model,
		Usage: llm.Usage{InputTokens: msg.Usage.InputTokens, OutputTokens: msg.Usage.OutputTokens},
	}, nil
}
