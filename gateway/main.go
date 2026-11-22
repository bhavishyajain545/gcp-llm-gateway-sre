// llm-gateway: an OpenAI-compatible front door for Vertex AI Gemini with tenant
// keys and quotas, a two-layer Redis cache, model fallback behind circuit
// breakers, Prometheus metrics for SLOs and autoscaling, and OpenTelemetry
// traces to Cloud Trace. Authentication to Google APIs is ambient (Workload
// Identity on GKE, ADC locally): the binary never sees a key file.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Version is set at build time: -ldflags "-X main.Version=…".
var Version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			switch a.Key { // Cloud Logging field names
			case slog.LevelKey:
				a.Key = "severity"
			case slog.MessageKey:
				a.Key = "message"
			}
			return a
		},
	})))
	cfg := LoadConfig()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := setupTracing(ctx, cfg, Version)
	if err != nil {
		slog.Error("tracing", "err", err)
		os.Exit(1)
	}

	faults := NewFaultInjector(nil) // Redis is attached below once the address is known
	vertex, err := NewVertexClient(ctx, cfg, faults)
	if err != nil {
		slog.Error("vertex client", "err", err)
		os.Exit(1)
	}
	secrets := &SecretReader{http: vertex.HTTPClient()}

	// Redis connection details come from Secret Manager on GKE (Terraform writes
	// them), from env locally. No Kubernetes Secret and nothing in the manifests.
	if cfg.RedisSecret != "" {
		b, err := secrets.Access(ctx, cfg.RedisSecret)
		if err != nil {
			slog.Error("redis secret", "err", err)
			os.Exit(1)
		}
		var rs struct {
			Addr     string `json:"addr"`
			Password string `json:"password"`
		}
		if err := json.Unmarshal(b, &rs); err != nil || rs.Addr == "" {
			slog.Error("redis secret must be JSON {addr, password}", "err", err)
			os.Exit(1)
		}
		cfg.RedisAddr, cfg.RedisPassword = rs.Addr, rs.Password
	}
	rdb := redis.NewClient(&redis.Options{
		Addr: cfg.RedisAddr, Password: cfg.RedisPassword,
		DialTimeout: 2 * time.Second, ReadTimeout: 500 * time.Millisecond, WriteTimeout: 500 * time.Millisecond,
		PoolSize: 50, MinIdleConns: 5,
	})
	faults.rdb = rdb // chaos faults are shared by every replica

	tenants := NewTenantStore()
	if cfg.TenantsJSON != "" {
		ts, err := parseTenants([]byte(cfg.TenantsJSON))
		if err != nil {
			slog.Error("TENANTS_JSON", "err", err)
			os.Exit(1)
		}
		tenants.Replace(ts)
	} else if cfg.TenantsSecret != "" {
		go runTenantRefresh(ctx, tenants, secrets, cfg.TenantsSecret, cfg.TenantsRefresh)
	} else {
		slog.Error("set TENANTS_JSON or TENANTS_SECRET")
		os.Exit(1)
	}
	if cfg.AdminKey == "" && cfg.AdminKeySecret != "" {
		b, err := secrets.Access(ctx, cfg.AdminKeySecret)
		if err != nil {
			slog.Error("admin key secret", "err", err)
			os.Exit(1)
		}
		cfg.AdminKey = strings.TrimSpace(string(b))
	}

	srv := &Server{
		cfg: cfg, version: Version, up: vertex,
		cache:   NewCache(rdb, cfg.CacheTTL, cfg.SemanticThreshold, cfg.SemanticMax),
		lim:     NewLimiter(rdb),
		router:  NewRouter(cfg),
		tenants: tenants, faults: faults, prices: loadPrices(cfg.PricesJSON), rdb: rdb,
	}
	handler := otelhttp.NewHandler(srv.routes(), "llm-gateway",
		otelhttp.WithFilter(func(r *http.Request) bool { // keep probe/scrape noise out of traces
			return r.URL.Path != "/metrics" && r.URL.Path != "/healthz" && r.URL.Path != "/readyz"
		}))
	hs := &http.Server{
		Addr: ":" + cfg.Port, Handler: handler,
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second,
	}
	go func() {
		slog.Info("listening", "port", cfg.Port, "version", Version, "models", cfg.Models, "default", cfg.DefaultModel,
			"semantic_cache", cfg.SemanticEnabled, "trace_exporter", cfg.TraceExporter)
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("serve", "err", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	slog.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 25*time.Second) // < terminationGracePeriodSeconds
	defer cancel()
	_ = hs.Shutdown(sctx)
	_ = shutdownTracing(sctx)
	_ = rdb.Close()
}
