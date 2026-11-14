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
