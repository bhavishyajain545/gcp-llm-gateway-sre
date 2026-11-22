package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is read from the environment once at start-up. Every knob has a
// production-sensible default so the binary runs locally with just a project.
type Config struct {
	Port    string
	Project string

	// Gemini
	Location        string // "global" (recommended for Gemini) or a region
	Models          []string
	DefaultModel    string
	Fallbacks       map[string]string // model -> model tried when the first one fails or its breaker is open
	ThinkingLevel   string            // sent as generationConfig.thinkingConfig.thinkingLevel for gemini-3* models
	UpstreamTimeout time.Duration

	// Embeddings (semantic cache)
	EmbedLocation string
	EmbedModel    string
	EmbedDims     int

	// Redis (Memorystore): cache + rate limits + daily budgets
	RedisAddr         string
	RedisPassword     string
	RedisSecret       string // Secret Manager version holding {"addr":"host:port","password":"…"} (set by Terraform)
	CacheTTL          time.Duration
	SemanticEnabled   bool
	SemanticThreshold float64
	SemanticMax       int64

	// Tenants / admin
	TenantsSecret  string // projects/P/secrets/NAME/versions/latest (Secret Manager), refreshed periodically
	TenantsJSON    string // inline JSON for local runs and tests
	TenantsRefresh time.Duration
	AdminKey       string
	AdminKeySecret string

	// Tracing
	TraceExporter string // "gcp" exports to Cloud Trace; anything else = no exporter
	TraceRatio    float64

	// Circuit breaker per model
	BreakerFailures int
	BreakerWindow   time.Duration
	BreakerOpen     time.Duration

	PricesJSON string
}

func LoadConfig() Config {
	c := Config{
		Port:              env("PORT", "8080"),
		Project:           env("GOOGLE_CLOUD_PROJECT", ""),
		Location:          env("GEMINI_LOCATION", "global"),
		Models:            splitList(env("MODELS", "gemini-3.8-flash,gemini-3.5-flash-lite")),
		DefaultModel:      env("DEFAULT_MODEL", "gemini-3.8-flash"),
		Fallbacks:         parsePairs(env("MODEL_FALLBACKS", "gemini-3.8-flash=gemini-3.5-flash-lite")),
		ThinkingLevel:     env("THINKING_LEVEL", "LOW"),
		UpstreamTimeout:   envDuration("UPSTREAM_TIMEOUT", 60*time.Second),
		EmbedLocation:     env("EMBED_LOCATION", "us-central1"),
		EmbedModel:        env("EMBED_MODEL", "gemini-embedding-001"),
		EmbedDims:         envInt("EMBED_DIMS", 256),
		RedisAddr:         env("REDIS_ADDR", "127.0.0.1:6379"),
		RedisPassword:     env("REDIS_PASSWORD", ""),
		RedisSecret:       env("REDIS_SECRET", ""),
		CacheTTL:          envDuration("CACHE_TTL", 6*time.Hour),
		SemanticEnabled:   envBool("SEMANTIC_CACHE", true),
		SemanticThreshold: envFloat("SEMANTIC_THRESHOLD", 0.95),
		SemanticMax:       int64(envInt("SEMANTIC_MAX_ENTRIES", 2000)),
		TenantsSecret:     env("TENANTS_SECRET", ""),
		TenantsJSON:       env("TENANTS_JSON", ""),
		TenantsRefresh:    envDuration("TENANTS_REFRESH", 60*time.Second),
		AdminKey:          env("ADMIN_KEY", ""),
		AdminKeySecret:    env("ADMIN_KEY_SECRET", ""),
		TraceExporter:     env("TRACE_EXPORTER", ""),
		TraceRatio:        envFloat("TRACE_RATIO", 0.2),
		BreakerFailures:   envInt("BREAKER_FAILURES", 5),
		BreakerWindow:     envDuration("BREAKER_WINDOW", 30*time.Second),
		BreakerOpen:       envDuration("BREAKER_OPEN", 30*time.Second),
		PricesJSON:        env("PRICES_JSON", ""),
	}
	return c
}

func (c Config) ModelKnown(m string) bool {
	for _, x := range c.Models {
		if x == m {
			return true
		}
	}
	return false
}

func env(k, d string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return d
}

func envInt(k string, d int) int {
	if v, err := strconv.Atoi(env(k, "")); err == nil {
		return v
	}
	return d
}

func envFloat(k string, d float64) float64 {
	if v, err := strconv.ParseFloat(env(k, ""), 64); err == nil {
		return v
	}
	return d
}

func envBool(k string, d bool) bool {
	if v, err := strconv.ParseBool(env(k, "")); err == nil {
		return v
	}
	return d
}

func envDuration(k string, d time.Duration) time.Duration {
	if v, err := time.ParseDuration(env(k, "")); err == nil {
		return v
	}
	return d
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parsePairs turns "a=b,c=d" into a map.
func parsePairs(s string) map[string]string {
	m := map[string]string{}
	for _, p := range splitList(s) {
		if k, v, ok := strings.Cut(p, "="); ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}
