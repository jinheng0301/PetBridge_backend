package config

import (
	"strings"
	"testing"
)

func TestLoad_PolishDefaults(t *testing.T) {
	c, err := Load(env(required()))
	if err != nil {
		t.Fatal(err)
	}
	if !c.AIPolishEnabled || c.AIPolishDailyQuota != 30 || c.ClaudeModelPolish != "claude-haiku-5-5" {
		t.Fatalf("polish defaults: enabled=%v quota=%d claude=%q", c.AIPolishEnabled, c.AIPolishDailyQuota, c.ClaudeModelPolish)
	}
	c, err = Load(env(with(required(), "AI_POLISH_ENABLED", "false", "AI_POLISH_DAILY_QUOTA", "0")))
	if err != nil || c.AIPolishEnabled || c.AIPolishDailyQuota != 0 {
		t.Fatalf("overrides: %+v %v", c, err)
	}
	for _, kv := range [][2]string{{"AI_POLISH_ENABLED", "maybe"}, {"AI_POLISH_DAILY_QUOTA", "-1"}, {"AI_POLISH_DAILY_QUOTA", "x"}} {
		if p := problems(t, errOf(Load(env(with(required(), kv[0], kv[1]))))); !strings.Contains(p, kv[0]) {
			t.Errorf("%s=%s: %s", kv[0], kv[1], p)
		}
	}
}

func TestLoad_GeminiPolishModelRequired(t *testing.T) {
	e := with(required(), "AI_PROVIDER", "live", "GEMINI_API_KEY", "secret-gem-key", "GEMINI_MODEL_TRANSLATE", "m")
	if p := problems(t, errOf(Load(env(e)))); !strings.Contains(p, "GEMINI_MODEL_POLISH") {
		t.Fatalf("problems: %s", p)
	}
	c, err := Load(env(with(e, "GEMINI_MODEL_POLISH", "mp")))
	if err != nil || c.GeminiModelPolish != "mp" {
		t.Fatalf("got %+v %v", c, err)
	}
}

func TestLoad_GeminiPriceHalfSetRejected(t *testing.T) {
	p := problems(t, errOf(Load(env(with(required(), "GEMINI_PRICE_IN_USD_PER_M", "0.1")))))
	if !strings.Contains(p, "GEMINI_PRICE_OUT_USD_PER_M") {
		t.Fatalf("problems: %s", p)
	}
}

func TestLoad_ProdClaudePolishModelNeedsPrice(t *testing.T) {
	e := with(required(),
		"APP_ENV", "production",
		"JWT_SIGNING_KEY", strings.Repeat("k", 32),
		"DATABASE_URL", "postgres://u:p@db/petbridge?sslmode=require",
		"AI_PROVIDER", "live", "AI_PRIMARY", "claude", "ANTHROPIC_API_KEY", "sk-ant-x",
		"CLAUDE_MODEL_POLISH", "claude-unpriced")
	if p := problems(t, errOf(Load(env(e)))); !strings.Contains(p, "CLAUDE_MODEL_POLISH") {
		t.Fatalf("problems: %s", p)
	}
}
