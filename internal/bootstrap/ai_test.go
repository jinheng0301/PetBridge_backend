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

func TestPrices(t *testing.T) {
	c := &config.Config{GeminiModelTranslate: "gemini-x", GeminiPriceSet: true, GeminiPriceInMicros: 1, GeminiPriceOutMicros: 2}
	if p := Prices(c); len(p) != 1 || p["gemini-x"] != (llm.Price{InPerM: 1, OutPerM: 2}) {
		t.Fatalf("prices: %v", p)
	}
	if p := Prices(&config.Config{GeminiModelTranslate: "gemini-x"}); len(p) != 0 {
		t.Fatalf("unset: %v", p)
	}
}

func TestPrices_CoversPolishModel(t *testing.T) {
	c := &config.Config{GeminiModelTranslate: "gemini-t", GeminiModelPolish: "gemini-p", GeminiPriceSet: true, GeminiPriceInMicros: 1, GeminiPriceOutMicros: 2}
	p := Prices(c)
	want := llm.Price{InPerM: 1, OutPerM: 2}
	if len(p) != 2 || p["gemini-t"] != want || p["gemini-p"] != want {
		t.Fatalf("prices: %v", p)
	}
}
