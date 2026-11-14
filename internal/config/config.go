// Package config loads and validates process configuration from environment
// variables. Validation errors name variables but never include their values.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	AppEnv          string
	AppPort         int
	LogLevel        string
	DatabaseURL     string
	RedisURL        string
	JWTSigningKey   string
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	DevLoginEnabled bool
	AIProvider      string
}

// ValidationError lists every configuration problem found. Problems contain
// variable names and reasons only, never values.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "invalid configuration: " + strings.Join(e.Problems, "; ")
}

// Load reads configuration through getenv only (never os.Environ).
func Load(getenv func(string) string) (*Config, error) {
	var problems []string
	add := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	get := func(name, def string) string {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			return v
		}
		return def
	}
	required := func(name string) string {
		v := strings.TrimSpace(getenv(name))
		if v == "" {
			add("%s is required", name)
		}
		return v
	}
	oneOf := func(name, v string, allowed ...string) {
		for _, a := range allowed {
			if v == a {
				return
			}
		}
		add("%s must be one of %s", name, strings.Join(allowed, "|"))
	}
	duration := func(name, def string) time.Duration {
		d, err := time.ParseDuration(get(name, def))
		if err != nil || d <= 0 {
			add("%s must be a positive duration such as 15m", name)
			return 0
		}
		return d
	}

	c := &Config{
		AppEnv:        get("APP_ENV", "local"),
		LogLevel:      get("LOG_LEVEL", "info"),
		AIProvider:    get("AI_PROVIDER", "fake"),
		DatabaseURL:   required("DATABASE_URL"),
		RedisURL:      required("REDIS_URL"),
		JWTSigningKey: required("JWT_SIGNING_KEY"),
	}
	oneOf("APP_ENV", c.AppEnv, "local", "staging", "production")
	oneOf("LOG_LEVEL", c.LogLevel, "debug", "info", "warn", "error")
	oneOf("AI_PROVIDER", c.AIProvider, "fake", "live")

	port, err := strconv.Atoi(get("APP_PORT", "8080"))
	if err != nil || port < 1 || port > 65535 {
		add("APP_PORT must be an integer between 1 and 65535")
	}
	c.AppPort = port

	c.AccessTokenTTL = duration("ACCESS_TOKEN_TTL", "15m")
	c.RefreshTokenTTL = duration("REFRESH_TOKEN_TTL", "720h")

	dev, err := strconv.ParseBool(get("DEV_LOGIN_ENABLED", "false"))
	if err != nil {
		add("DEV_LOGIN_ENABLED must be true or false")
	}
	c.DevLoginEnabled = dev

	if c.AppEnv != "local" && c.DevLoginEnabled {
		add("DEV_LOGIN_ENABLED must be false unless APP_ENV=local")
	}
	if c.AppEnv == "production" {
		if c.AIProvider != "live" {
			add("AI_PROVIDER must be live when APP_ENV=production")
		}
		if c.JWTSigningKey != "" && len(c.JWTSigningKey) < 32 {
			add("JWT_SIGNING_KEY must be at least 32 characters when APP_ENV=production")
		}
	}

	if len(problems) > 0 {
		return nil, &ValidationError{Problems: problems}
	}
	return c, nil
}

// LoadFromProcess loads a local .env file (without overriding existing
// variables) when APP_ENV is unset or local, then calls Load(os.Getenv).
func LoadFromProcess() (*Config, error) {
	if env := os.Getenv("APP_ENV"); env == "" || env == "local" {
		if _, err := os.Stat(".env"); err == nil {
			_ = godotenv.Load(".env") // Load never overrides existing vars
		}
	}
	return Load(os.Getenv)
}
