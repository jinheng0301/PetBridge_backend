package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func required() map[string]string {
	return map[string]string{
		"APP_ENV":         "local",
		"DATABASE_URL":    "postgres://petbridge:petbridge@localhost:5432/petbridge",
		"REDIS_URL":       "redis://localhost:6379/0",
		"JWT_SIGNING_KEY": "local-dev-only-change-me",
	}
}

func with(base map[string]string, kv ...string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

func problems(t *testing.T, err error) string {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want *ValidationError, got %T: %v", err, err)
	}
	return strings.Join(ve.Problems, "\n")
}

func TestLoad_Defaults(t *testing.T) {
	c, err := Load(env(required()))
	if err != nil {
		t.Fatal(err)
	}
	if c.AppEnv != "local" || c.AppPort != 8080 || c.LogLevel != "info" ||
		c.AccessTokenTTL != 15*time.Minute || c.RefreshTokenTTL != 720*time.Hour ||
		c.AIProvider != "fake" || c.DevLoginEnabled {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestLoad_MissingRequired_ListsAllNames(t *testing.T) {
	_, err := Load(env(nil))
	p := problems(t, err)
	for _, n := range []string{"DATABASE_URL", "REDIS_URL", "JWT_SIGNING_KEY"} {
		if !strings.Contains(p, n) {
			t.Errorf("problems missing %s: %s", n, p)
		}
	}
}

func TestLoad_BlankIsMissing(t *testing.T) {
	for _, name := range []string{"DATABASE_URL", "REDIS_URL", "JWT_SIGNING_KEY"} {
		for _, v := range []string{"", "   "} {
			_, err := Load(env(with(required(), name, v)))
			if p := problems(t, err); !strings.Contains(p, name) {
				t.Errorf("%s=%q not reported: %s", name, v, p)
			}
		}
	}
}

func TestLoad_ProductionSelfCheck(t *testing.T) {
	ok := with(required(), "APP_ENV", "production", "AI_PROVIDER", "live",
		"AI_PRIMARY", "claude", "ANTHROPIC_API_KEY", "sk-ant-x",
		"DEV_LOGIN_ENABLED", "false", "JWT_SIGNING_KEY", strings.Repeat("k", 32))
	if _, err := Load(env(ok)); err != nil {
		t.Fatalf("valid production config rejected: %v", err)
	}
	cases := map[string]map[string]string{
		"DEV_LOGIN_ENABLED": with(ok, "DEV_LOGIN_ENABLED", "true"),
		"AI_PROVIDER":       with(ok, "AI_PROVIDER", "fake"),
		"JWT_SIGNING_KEY":   with(ok, "JWT_SIGNING_KEY", strings.Repeat("k", 31)),
	}
	for name, m := range cases {
		_, err := Load(env(m))
		if p := problems(t, err); !strings.Contains(p, name) {
			t.Errorf("%s not reported: %s", name, p)
		}
	}
}

func TestLoad_AppEnvIsRequired(t *testing.T) {
	m := required()
	delete(m, "APP_ENV")
	_, err := Load(env(m))
	if p := problems(t, err); !strings.Contains(p, "APP_ENV") {
		t.Fatalf("not reported: %s", p)
	}
}

func TestLoad_NonLocalRequiresStrongKeyAndTLS(t *testing.T) {
	good := with(required(), "APP_ENV", "staging", "JWT_SIGNING_KEY", strings.Repeat("k", 32))
	if _, err := Load(env(good)); err != nil {
		t.Fatalf("valid staging config rejected: %v", err)
	}
	short := with(good, "JWT_SIGNING_KEY", "local-dev-only-change-me")
	if p := problems(t, errOf(Load(env(short)))); !strings.Contains(p, "JWT_SIGNING_KEY") {
		t.Errorf("short staging key not reported: %s", p)
	}
	noTLS := with(good, "DATABASE_URL", "postgres://u:p@h/db?sslmode=disable")
	if p := problems(t, errOf(Load(env(noTLS)))); !strings.Contains(p, "DATABASE_URL") {
		t.Errorf("sslmode=disable not reported: %s", p)
	}
}

func errOf(_ *Config, err error) error { return err }

func TestLoad_HostDefaults(t *testing.T) {
	c, _ := Load(env(required()))
	if c.AppHost != "127.0.0.1" {
		t.Errorf("local host = %q, want 127.0.0.1", c.AppHost)
	}
	c, err := Load(env(with(required(), "APP_ENV", "staging", "JWT_SIGNING_KEY", strings.Repeat("k", 32))))
	if err != nil || c.AppHost != "0.0.0.0" {
		t.Errorf("staging host = %v, err %v", c, err)
	}
	c, _ = Load(env(with(required(), "APP_HOST", "0.0.0.0")))
	if c.AppHost != "0.0.0.0" {
		t.Errorf("override ignored: %q", c.AppHost)
	}
}

func TestLoad_DevLoginOnlyInLocal(t *testing.T) {
	_, err := Load(env(with(required(), "APP_ENV", "staging", "DEV_LOGIN_ENABLED", "true")))
	if p := problems(t, err); !strings.Contains(p, "DEV_LOGIN_ENABLED") {
		t.Fatalf("not reported: %s", p)
	}
}

func TestLoad_ErrorNeverContainsSecrets(t *testing.T) {
	m := with(required(), "APP_ENV", "production", "AI_PROVIDER", "fake",
		"JWT_SIGNING_KEY", "super-secret-value",
		"DATABASE_URL", "postgres://u:hunter2@h/db", "APP_PORT", "hunter2")
	_, err := Load(env(m))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, s := range []string{"super-secret-value", "hunter2"} {
		if strings.Contains(err.Error(), s) {
			t.Errorf("error leaks %q: %v", s, err)
		}
	}
}

func TestLoad_InvalidValues(t *testing.T) {
	cases := map[string]string{
		"APP_PORT":         "abc",
		"ACCESS_TOKEN_TTL": "zzz",
		"APP_ENV":          "prod",
		"LOG_LEVEL":        "loud",
	}
	for name, v := range cases {
		_, err := Load(env(with(required(), name, v)))
		if p := problems(t, err); !strings.Contains(p, name) {
			t.Errorf("%s=%q not reported: %s", name, v, p)
		}
	}
	_, err := Load(env(with(required(), "APP_PORT", "70000")))
	if p := problems(t, err); !strings.Contains(p, "APP_PORT") {
		t.Errorf("APP_PORT=70000 not reported: %s", p)
	}
}

func TestLoad_CodeTTLDaysDefault(t *testing.T) {
	c, err := Load(env(required()))
	if err != nil {
		t.Fatal(err)
	}
	if c.CodeTTLDays != 7 {
		t.Fatalf("CodeTTLDays = %d, want 7", c.CodeTTLDays)
	}
}

func TestLoad_CodeTTLDaysInvalid(t *testing.T) {
	for _, v := range []string{"0", "366", "abc"} {
		_, err := Load(env(with(required(), "CODE_TTL_DAYS", v)))
		p := problems(t, err)
		if !strings.Contains(p, "CODE_TTL_DAYS") {
			t.Errorf("value %q: problems %q should name CODE_TTL_DAYS", v, p)
		}
		if strings.Contains(p, v) && v != "0" {
			t.Errorf("value %q leaked into problems %q", v, p)
		}
	}
}

func TestLoadDB_OnlyNeedsEnvAndDatabaseURL(t *testing.T) {
	c, err := LoadDB(env(map[string]string{"APP_ENV": "local", "DATABASE_URL": "postgres://u:p@h/db?sslmode=disable"}))
	if err != nil || c.AppEnv != "local" {
		t.Fatalf("got %+v, %v", c, err)
	}
	for name, m := range map[string]map[string]string{
		"APP_ENV":      {"DATABASE_URL": "postgres://u:p@h/db"},
		"DATABASE_URL": {"APP_ENV": "local"},
	} {
		_, err := LoadDB(env(m))
		if p := problems(t, err); !strings.Contains(p, name) {
			t.Errorf("%s not reported: %s", name, p)
		}
	}
	_, err = LoadDB(env(map[string]string{"APP_ENV": "staging", "DATABASE_URL": "postgres://u:p@h/db?sslmode=disable"}))
	if p := problems(t, err); !strings.Contains(p, "sslmode") {
		t.Errorf("sslmode=disable in staging not reported: %s", p)
	}
}

func TestLoad_RedisURLMustBeWellFormed(t *testing.T) {
	for _, v := range []string{"redis//host:6379", "http://host:6379", "host:6379", "redis://%zz"} {
		_, err := Load(env(with(required(), "REDIS_URL", v)))
		p := problems(t, err)
		if !strings.Contains(p, "REDIS_URL") || strings.Contains(p, v) {
			t.Errorf("REDIS_URL=%q: %s", v, p)
		}
	}
	for _, v := range []string{"redis://localhost:6379/0", "rediss://:pw@host:6380/1"} {
		if _, err := Load(env(with(required(), "REDIS_URL", v))); err != nil {
			t.Errorf("REDIS_URL=%q rejected: %v", v, err)
		}
	}
}

func TestLoad_BusinessDefaults(t *testing.T) {
	c, err := Load(env(required()))
	if err != nil {
		t.Fatal(err)
	}
	if c.RewardCodeCount != 3 || c.RewardCodeLifetimeCap != 20 || c.OwnerRegenDailyLimit != 3 || c.RoomCapacity != 5 {
		t.Fatalf("unexpected business defaults: %d/%d/%d/%d",
			c.RewardCodeCount, c.RewardCodeLifetimeCap, c.OwnerRegenDailyLimit, c.RoomCapacity)
	}
}

func TestLoad_BusinessValuesInvalid(t *testing.T) {
	cases := []struct{ name, val string }{
		{"ROOM_CAPACITY", "1"},
		{"ROOM_CAPACITY", "6"},
		{"ROOM_CAPACITY", "x"},
		{"REWARD_CODE_COUNT", "-1"},
		{"REWARD_CODE_COUNT", "6"},
		{"REWARD_CODE_COUNT", "x"},
		{"REWARD_CODE_LIFETIME_CAP", "0"},
		{"OWNER_REGEN_DAILY_LIMIT", "0"},
	}
	for _, tc := range cases {
		_, err := Load(env(with(required(), tc.name, tc.val)))
		p := problems(t, err)
		if !strings.Contains(p, tc.name) {
			t.Errorf("%s=%q not reported: %s", tc.name, tc.val, p)
		}
		if strings.Contains(p, tc.val) {
			t.Errorf("%s=%q: problems leak the value: %s", tc.name, tc.val, p)
		}
	}
}

func TestLoad_RewardCountZeroAllowed(t *testing.T) {
	c, err := Load(env(with(required(), "REWARD_CODE_COUNT", "0")))
	if err != nil {
		t.Fatalf("REWARD_CODE_COUNT=0 rejected: %v", err)
	}
	if c.RewardCodeCount != 0 {
		t.Fatalf("RewardCodeCount = %d, want 0", c.RewardCodeCount)
	}
}

func TestLoad_GoogleClientID(t *testing.T) {
	c, err := Load(env(required()))
	if err != nil {
		t.Fatal(err)
	}
	if c.GoogleClientID != "" {
		t.Fatalf("GoogleClientID default = %q, want empty", c.GoogleClientID)
	}
	c, err = Load(env(with(required(), "GOOGLE_CLIENT_ID", "  web-123.apps.googleusercontent.com ")))
	if err != nil {
		t.Fatal(err)
	}
	if c.GoogleClientID != "web-123.apps.googleusercontent.com" {
		t.Fatalf("GoogleClientID = %q", c.GoogleClientID)
	}
}

func TestLoad_MinAppVersion(t *testing.T) {
	c, err := Load(env(required()))
	if err != nil {
		t.Fatal(err)
	}
	if c.MinAppVersion != "0.0.0" {
		t.Fatalf("MinAppVersion default = %q, want 0.0.0", c.MinAppVersion)
	}
	c, err = Load(env(with(required(), "MIN_APP_VERSION", "1.4.2")))
	if err != nil || c.MinAppVersion != "1.4.2" {
		t.Fatalf("MinAppVersion = %q, err=%v", c.MinAppVersion, err)
	}
	for _, bad := range []string{"abc", "1.2", "1.2.3-beta", "v1.2.3"} {
		_, err = Load(env(with(required(), "MIN_APP_VERSION", bad)))
		var ve *ValidationError
		if !errors.As(err, &ve) || !strings.Contains(err.Error(), "MIN_APP_VERSION must be a version such as 1.2.0") {
			t.Fatalf("MIN_APP_VERSION=%q: err=%v", bad, err)
		}
	}
}

func TestLoad_AIDefaults(t *testing.T) {
	c, err := Load(env(required()))
	if err != nil {
		t.Fatal(err)
	}
	if c.AIPrimary != "gemini" || !c.GeminiFreeTier || c.ClaudeModelTranslate != "claude-haiku-5-5" ||
		c.AIDailyBudgetMicros != 5_000_000 || c.WorkerConcurrency != 10 ||
		c.FakeAILatency != 300*time.Millisecond {
		t.Fatalf("unexpected AI defaults: %+v", c)
	}
}

func TestLoad_AIValidation(t *testing.T) {
	prod := with(required(),
		"APP_ENV", "production",
		"JWT_SIGNING_KEY", strings.Repeat("k", 32),
		"DATABASE_URL", "postgres://u:p@db/petbridge?sslmode=require",
		"AI_PROVIDER", "live")
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"bad primary", with(required(), "AI_PRIMARY", "openai"), "AI_PRIMARY"},
		{"live gemini no key", with(required(), "AI_PROVIDER", "live"), "GEMINI_API_KEY"},
		{"gemini key no model", with(required(), "AI_PROVIDER", "live", "GEMINI_API_KEY", "secret-gem-key"), "GEMINI_MODEL_TRANSLATE"},
		{"live claude no key", with(required(), "AI_PROVIDER", "live", "AI_PRIMARY", "claude"), "ANTHROPIC_API_KEY"},
		{"prod free tier", with(prod, "GEMINI_API_KEY", "secret-gem-key", "GEMINI_MODEL_TRANSLATE", "m"), "GEMINI_FREE_TIER"},
		{"fail rate", with(required(), "FAKE_AI_FAIL_RATE", "1.5"), "FAKE_AI_FAIL_RATE"},
		{"budget", with(required(), "AI_DAILY_BUDGET_USD", "0"), "AI_DAILY_BUDGET_USD"},
		{"concurrency", with(required(), "WORKER_CONCURRENCY", "0"), "WORKER_CONCURRENCY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(env(tc.env))
			p := problems(t, err)
			if !strings.Contains(p, tc.want) {
				t.Errorf("problems missing %s: %s", tc.want, p)
			}
			if strings.Contains(p, "secret-gem-key") {
				t.Errorf("problems leak a key value: %s", p)
			}
		})
	}
}

func TestLoad_ProductionClaudeOnly(t *testing.T) {
	e := with(required(),
		"APP_ENV", "production",
		"JWT_SIGNING_KEY", strings.Repeat("k", 32),
		"DATABASE_URL", "postgres://u:p@db/petbridge?sslmode=require",
		"AI_PROVIDER", "live", "AI_PRIMARY", "claude", "ANTHROPIC_API_KEY", "sk-ant-x")
	if _, err := Load(env(e)); err != nil {
		t.Fatal(err)
	}
}
