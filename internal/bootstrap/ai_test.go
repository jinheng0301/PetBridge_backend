package bootstrap

import (
	"context"
	"reflect"
	"testing"

	"github.com/jinheng0301/petbridge/internal/config"
	"github.com/jinheng0301/petbridge/pkg/llm"
)

func names(ps []llm.Provider) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Name())
	}
	return out
}

func TestProviders(t *testing.T) {
	cases := []struct {
		name string
		c    config.Config
		want []string
	}{
		{"fake", config.Config{AIProvider: "fake"}, []string{"fake-primary", "fake-secondary"}},
		{"live claude primary", config.Config{AIProvider: "live", AIPrimary: "claude", AnthropicAPIKey: "a", GeminiAPIKey: "g", GeminiModelTranslate: "m"}, []string{"claude", "gemini"}},
		{"live gemini primary no anthropic", config.Config{AIProvider: "live", AIPrimary: "gemini", GeminiAPIKey: "g", GeminiModelTranslate: "m"}, []string{"gemini"}},
		{"live gemini primary with anthropic", config.Config{AIProvider: "live", AIPrimary: "gemini", AnthropicAPIKey: "a", GeminiAPIKey: "g", GeminiModelTranslate: "m"}, []string{"gemini", "claude"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ps, err := Providers(context.Background(), &tc.c)
			if err != nil {
				t.Fatal(err)
			}
			if got := names(ps); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestZeroCost(t *testing.T) {
	if z := ZeroCost(&config.Config{AIProvider: "fake"}); !z["fake-primary"] || !z["fake-secondary"] {
		t.Fatalf("fake: %v", z)
	}
	if z := ZeroCost(&config.Config{AIProvider: "live", GeminiFreeTier: true}); !z["gemini"] || len(z) != 1 {
		t.Fatalf("free tier: %v", z)
	}
	if z := ZeroCost(&config.Config{AIProvider: "live"}); len(z) != 0 {
		t.Fatalf("paid: %v", z)
	}
}
