// Package config loads and validates process configuration from environment
// variables. Validation errors name variables but never include their values.
package config

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// versionRE matches a plain MAJOR.MINOR.PATCH app version.
var versionRE = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

type Config struct {
	AppEnv                  string
	AppHost                 string // listen address; 127.0.0.1 by default when APP_ENV=local
	AppPort                 int
	LogLevel                string
	DatabaseURL             string
	RedisURL                string
	JWTSigningKey           string
	AccessTokenTTL          time.Duration
	RefreshTokenTTL         time.Duration
	DevLoginEnabled         bool
	CodeTTLDays             int
	RoomCapacity            int // seats per room, owner included; 2..5 (room_members.seat_no is 1..5)
	RewardCodeCount         int // reward invite codes granted per successful join; 0 disables
	RewardCodeLifetimeCap   int // max reward codes a user can ever receive
	OwnerRegenDailyLimit    int // owner code regenerations allowed per day
	AIProvider              string
	AIPrimary               string // gemini | claude: which provider is tried first in live mode
	GeminiAPIKey            string
	GeminiModelTranslate    string
	GeminiFreeTier          bool // Gemini free tier: its cost is recorded as 0
	AnthropicAPIKey         string
	ClaudeModelTranslate    string
	FakeAILatency           time.Duration
	FakeAIFailRate          float64 // 0..1, fake-primary
	FakeAISecondaryFailRate float64 // 0..1, fake-secondary
	AIDailyBudgetMicros     int64   // AI_DAILY_BUDGET_USD in millionths of a dollar; > 0
	WorkerConcurrency       int     // 1..100
	GoogleClientID          string  // OAuth Web client ID; empty keeps Google login not configured
	MinAppVersion           string  // sent in the WebSocket hello as min_app_version
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
		AppEnv:         required("APP_ENV"), // no default: a missing value must not silently relax checks
		LogLevel:       get("LOG_LEVEL", "info"),
		AIProvider:     get("AI_PROVIDER", "fake"),
		DatabaseURL:    required("DATABASE_URL"),
		RedisURL:       required("REDIS_URL"),
		JWTSigningKey:  required("JWT_SIGNING_KEY"),
		GoogleClientID: get("GOOGLE_CLIENT_ID", ""),
	}
	if c.AppEnv != "" {
		oneOf("APP_ENV", c.AppEnv, "local", "staging", "production")
	}
	defHost := "0.0.0.0"
	if c.AppEnv == "local" {
		// Dev login has no password: keep local servers off the LAN unless asked.
		defHost = "127.0.0.1"
	}
	c.AppHost = get("APP_HOST", defHost)
	oneOf("LOG_LEVEL", c.LogLevel, "debug", "info", "warn", "error")
	oneOf("AI_PROVIDER", c.AIProvider, "fake", "live")

	port, err := strconv.Atoi(get("APP_PORT", "8080"))
	if err != nil || port < 1 || port > 65535 {
		add("APP_PORT must be an integer between 1 and 65535")
	}
	c.AppPort = port

	c.AccessTokenTTL = duration("ACCESS_TOKEN_TTL", "15m")
	c.RefreshTokenTTL = duration("REFRESH_TOKEN_TTL", "720h")

	ttlDays, err := strconv.Atoi(get("CODE_TTL_DAYS", "7"))
	if err != nil || ttlDays < 1 || ttlDays > 365 {
		add("CODE_TTL_DAYS must be an integer between 1 and 365")
	}
	c.CodeTTLDays = ttlDays

	capacity, err := strconv.Atoi(get("ROOM_CAPACITY", "5"))
	if err != nil || capacity < 2 || capacity > 5 {
		add("ROOM_CAPACITY must be an integer between 2 and 5")
	}
	c.RoomCapacity = capacity

	rewardCount, err := strconv.Atoi(get("REWARD_CODE_COUNT", "3"))
	if err != nil || rewardCount < 0 || rewardCount > 5 {
		add("REWARD_CODE_COUNT must be an integer between 0 and 5")
	}
	c.RewardCodeCount = rewardCount

	rewardCap, err := strconv.Atoi(get("REWARD_CODE_LIFETIME_CAP", "20"))
	if err != nil || rewardCap < 1 {
		add("REWARD_CODE_LIFETIME_CAP must be an integer of at least 1")
	}
	c.RewardCodeLifetimeCap = rewardCap

	regenLimit, err := strconv.Atoi(get("OWNER_REGEN_DAILY_LIMIT", "3"))
	if err != nil || regenLimit < 1 {
		add("OWNER_REGEN_DAILY_LIMIT must be an integer of at least 1")
	}
	c.OwnerRegenDailyLimit = regenLimit

	c.AIPrimary = get("AI_PRIMARY", "gemini")
	oneOf("AI_PRIMARY", c.AIPrimary, "gemini", "claude")
	c.GeminiAPIKey = get("GEMINI_API_KEY", "")
	c.GeminiModelTranslate = get("GEMINI_MODEL_TRANSLATE", "")
	c.AnthropicAPIKey = get("ANTHROPIC_API_KEY", "")
	c.ClaudeModelTranslate = get("CLAUDE_MODEL_TRANSLATE", "claude-haiku-5-5")
	freeTier, err := strconv.ParseBool(get("GEMINI_FREE_TIER", "true"))
	if err != nil {
		add("GEMINI_FREE_TIER must be true or false")
	}
	c.GeminiFreeTier = freeTier
	latencyMS, err := strconv.Atoi(get("FAKE_AI_LATENCY_MS", "300"))
	if err != nil || latencyMS < 0 {
		add("FAKE_AI_LATENCY_MS must be an integer of at least 0")
	}
	c.FakeAILatency = time.Duration(latencyMS) * time.Millisecond
	rate := func(name string) float64 {
		f, err := strconv.ParseFloat(get(name, "0"), 64)
		if err != nil || f < 0 || f > 1 {
			add("%s must be a number between 0 and 1", name)
			return 0
		}
		return f
	}
	c.FakeAIFailRate = rate("FAKE_AI_FAIL_RATE")
	c.FakeAISecondaryFailRate = rate("FAKE_AI_SECONDARY_FAIL_RATE")
	budget, err := strconv.ParseFloat(get("AI_DAILY_BUDGET_USD", "5"), 64)
	if err != nil || budget <= 0 || budget > 1e9 {
		add("AI_DAILY_BUDGET_USD must be a number greater than 0")
	} else {
		c.AIDailyBudgetMicros = int64(math.Round(budget * 1_000_000))
	}
	workers, err := strconv.Atoi(get("WORKER_CONCURRENCY", "10"))
	if err != nil || workers < 1 || workers > 100 {
		add("WORKER_CONCURRENCY must be an integer between 1 and 100")
	}
	c.WorkerConcurrency = workers

	c.MinAppVersion = get("MIN_APP_VERSION", "0.0.0")
	if !versionRE.MatchString(c.MinAppVersion) {
		add("MIN_APP_VERSION must be a version such as 1.2.0")
	}

	dev, err := strconv.ParseBool(get("DEV_LOGIN_ENABLED", "false"))
	if err != nil {
		add("DEV_LOGIN_ENABLED must be true or false")
	}
	c.DevLoginEnabled = dev

	if c.RedisURL != "" {
		// A malformed URL is a deployment mistake, not an outage: fail fast
		// instead of silently running without Redis.
		if u, err := url.Parse(c.RedisURL); err != nil || (u.Scheme != "redis" && u.Scheme != "rediss" && u.Scheme != "unix") {
			add("REDIS_URL must be a redis://, rediss:// or unix:// URL")
		}
	}

	if c.AppEnv != "local" && c.DevLoginEnabled {
		add("DEV_LOGIN_ENABLED must be false unless APP_ENV=local")
	}
	if c.AppEnv == "production" && c.AIProvider != "live" {
		add("AI_PROVIDER must be live when APP_ENV=production")
	}
	if c.AIProvider == "live" {
		geminiOnChain := c.AIPrimary == "gemini" || c.GeminiAPIKey != ""
		if c.AIPrimary == "gemini" && c.GeminiAPIKey == "" {
			add("GEMINI_API_KEY is required when AI_PROVIDER=live and AI_PRIMARY=gemini")
		}
		if c.GeminiAPIKey != "" && c.GeminiModelTranslate == "" {
			add("GEMINI_MODEL_TRANSLATE is required when GEMINI_API_KEY is set")
		}
		if c.AIPrimary == "claude" && c.AnthropicAPIKey == "" {
			add("ANTHROPIC_API_KEY is required when AI_PROVIDER=live and AI_PRIMARY=claude")
		}
		if c.AppEnv == "production" && geminiOnChain && c.GeminiFreeTier {
			add("GEMINI_FREE_TIER must be false when APP_ENV=production and Gemini is in the provider chain")
		}
	}
	if c.AppEnv != "" && c.AppEnv != "local" {
		if c.JWTSigningKey != "" && len(c.JWTSigningKey) < 32 {
			add("JWT_SIGNING_KEY must be at least 32 characters unless APP_ENV=local")
		}
		if strings.Contains(strings.ToLower(c.DatabaseURL), "sslmode=disable") {
			add("DATABASE_URL must not use sslmode=disable unless APP_ENV=local")
		}
	}

	if len(problems) > 0 {
		return nil, &ValidationError{Problems: problems}
	}
	return c, nil
}

// DBConfig is the subset of configuration that database tooling needs, so
// migrate does not demand REDIS_URL or JWT_SIGNING_KEY.
type DBConfig struct {
	AppEnv      string
	DatabaseURL string
}

// LoadDB reads only APP_ENV and DATABASE_URL.
func LoadDB(getenv func(string) string) (*DBConfig, error) {
	var problems []string
	c := &DBConfig{AppEnv: strings.TrimSpace(getenv("APP_ENV")), DatabaseURL: strings.TrimSpace(getenv("DATABASE_URL"))}
	switch c.AppEnv {
	case "local", "staging", "production":
	case "":
		problems = append(problems, "APP_ENV is required")
	default:
		problems = append(problems, "APP_ENV must be one of local|staging|production")
	}
	if c.DatabaseURL == "" {
		problems = append(problems, "DATABASE_URL is required")
	} else if c.AppEnv != "" && c.AppEnv != "local" && strings.Contains(strings.ToLower(c.DatabaseURL), "sslmode=disable") {
		problems = append(problems, "DATABASE_URL must not use sslmode=disable unless APP_ENV=local")
	}
	if len(problems) > 0 {
		return nil, &ValidationError{Problems: problems}
	}
	return c, nil
}

func loadDotenv() {
	if env := os.Getenv("APP_ENV"); env == "" || env == "local" {
		if _, err := os.Stat(".env"); err == nil {
			_ = godotenv.Load(".env") // Load never overrides existing vars
		}
	}
}

// LoadDBFromProcess is LoadFromProcess for database tooling.
func LoadDBFromProcess() (*DBConfig, error) {
	loadDotenv()
	return LoadDB(os.Getenv)
}

// LoadFromProcess loads a local .env file (without overriding existing
// variables) when APP_ENV is unset or local, then calls Load(os.Getenv).
func LoadFromProcess() (*Config, error) {
	loadDotenv()
	return Load(os.Getenv)
}
