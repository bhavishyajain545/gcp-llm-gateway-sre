package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metric design: everything the SLOs, the HPA and the cost dashboard need, with
// low-cardinality labels only (tenant and model are bounded sets).
var (
	requests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_requests_total",
		Help: "Chat completion requests by tenant, model served and outcome (ok, fallback_ok, cache_hit, rate_limited, budget_exhausted, client_error, unauthorized, upstream_error, internal_error).",
	}, []string{"tenant", "model", "outcome"})

	duration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "llm_request_duration_seconds",
		Help:    "End-to-end gateway latency.",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 3, 5, 8, 13, 20, 30, 60},
	}, []string{"model", "stream"})

	ttft = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "llm_ttft_seconds",
		Help:    "Time to first streamed token as seen by the client (streaming requests only, cache hits included).",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 0.75, 1, 1.5, 2, 3, 5, 8, 13},
	}, []string{"model"})

	tokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_tokens_total",
		Help: "Tokens billed by the provider (kind = prompt | completion; thinking tokens count as completion).",
	}, []string{"tenant", "model", "kind"})

	costUSD = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_cost_usd_total",
		Help: "Estimated provider cost from the price table (USD).",
	}, []string{"tenant", "model"})

	cacheEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_cache_events_total",
		Help: "Cache lookups by layer (exact, semantic) and result (hit, miss, error).",
	}, []string{"layer", "result"})

	cacheScore = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "llm_semantic_cache_score",
		Help:    "Best cosine score seen on semantic lookups (hits and misses).",
		Buckets: []float64{0.5, 0.6, 0.7, 0.8, 0.85, 0.9, 0.93, 0.95, 0.97, 0.99, 1},
	})

	inflight = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "llm_inflight_requests",
		Help: "Requests currently being handled by this replica (HPA signal).",
	})

	upstreamRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_upstream_requests_total",
		Help: "Calls to Vertex AI by model and HTTP status.",
	}, []string{"model", "code"})

	upstreamLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "llm_upstream_duration_seconds",
		Help:    "Vertex AI call latency (full response).",
		Buckets: prometheus.ExponentialBuckets(0.1, 1.6, 14),
	}, []string{"model"})

	upstreamTTFT = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "llm_upstream_ttft_seconds",
		Help:    "Vertex AI time to first streamed chunk.",
		Buckets: []float64{0.1, 0.25, 0.5, 0.75, 1, 1.5, 2, 3, 5, 8},
	}, []string{"model"})

	fallbacks = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_fallbacks_total",
		Help: "Requests moved from one model to another after a failure or an open breaker.",
	}, []string{"from", "to"})

	breakerOpen = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "llm_breaker_open",
		Help: "1 while the circuit breaker for a model is open.",
	}, []string{"model"})

	breakerSkips = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_breaker_skips_total",
		Help: "Requests that skipped a model because its breaker was open.",
	}, []string{"model"})

	rateLimited = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_rate_limited_total",
		Help: "Requests rejected by tenant limits (reason = rpm | daily_tokens).",
	}, []string{"tenant", "reason"})

	limiterErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_limiter_errors_total",
		Help: "Redis errors in the limiter; the gateway fails open and counts them here.",
	}, []string{"op"})

	embedRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_embed_requests_total",
		Help: "Embedding calls for the semantic cache by HTTP status.",
	}, []string{"code"})

	embedLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "llm_embed_duration_seconds",
		Help:    "Embedding call latency.",
		Buckets: []float64{0.05, 0.1, 0.2, 0.3, 0.5, 0.75, 1, 2, 5},
	})

	faultsApplied = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_faults_applied_total",
		Help: "Operator fault injections applied (chaos drills).",
	}, []string{"model", "mode"})

	tenantRefresh = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_tenant_refresh_total",
		Help: "Tenant list reloads from Secret Manager.",
	}, []string{"result"})
)
