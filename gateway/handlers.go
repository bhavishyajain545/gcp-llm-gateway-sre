package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type Server struct {
	cfg     Config
	version string
	up      Upstream
	cache   *Cache
	lim     *Limiter
	router  *Router
	tenants *TenantStore
	faults  *FaultInjector
	prices  Prices
	rdb     *redis.Client
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]string{"version": s.version, "default_model": s.cfg.DefaultModel})
	})
	// /metrics is for the in-cluster Prometheus collector only. Requests that arrived through the
	// Google front end carry "Via: 1.1 google"; refuse those so tenant names and volumes stay private.
	metrics := promhttp.Handler()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(strings.ToLower(r.Header.Get("Via")), "google") {
			http.NotFound(w, r)
			return
		}
		metrics.ServeHTTP(w, r)
	})
	mux.HandleFunc("POST /admin/chaos", s.handleChaos)
	mux.HandleFunc("GET /admin/chaos", s.handleChaosList)
	return mux
}

// --- helpers ----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, typ, msg string) {
	var e apiError
	e.Error.Message, e.Error.Type, e.Error.Code = msg, typ, typ
	writeJSON(w, code, e)
}

func (s *Server) authenticate(r *http.Request) (*Tenant, bool) {
	key := r.Header.Get("x-api-key")
	if key == "" {
		if h := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(h), "bearer ") {
			key = strings.TrimSpace(h[7:])
		}
	}
	return s.tenants.Lookup(key)
}

func logCtx(ctx context.Context, project string) []any {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return []any{
		"logging.googleapis.com/trace", "projects/" + project + "/traces/" + sc.TraceID().String(),
		"logging.googleapis.com/spanId", sc.SpanID().String(),
		"logging.googleapis.com/trace_sampled", sc.IsSampled(),
	}
}

// --- /readyz ------------------------------------------------------------------

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if !s.tenants.Loaded() {
		http.Error(w, "tenants not loaded", http.StatusServiceUnavailable)
		return
	}
	if err := s.rdb.Ping(ctx).Err(); err != nil {
		http.Error(w, "redis: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ready"))
}

// --- /v1/models ---------------------------------------------------------------

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	t, ok := s.authenticate(r)
	if !ok {
		writeError(w, 401, "invalid_api_key", "missing or invalid API key")
		return
	}
	type model struct {
		ID       string  `json:"id"`
		Object   string  `json:"object"`
		OwnedBy  string  `json:"owned_by"`
		Fallback string  `json:"fallback,omitempty"`
		InPer1M  float64 `json:"input_usd_per_1m"`
		OutPer1M float64 `json:"output_usd_per_1m"`
	}
	var out []model
	for _, m := range s.cfg.Models {
		if !t.Allows(m) {
			continue
		}
		p := s.prices[m]
		out = append(out, model{ID: m, Object: "model", OwnedBy: "google", Fallback: s.cfg.Fallbacks[m], InPer1M: p.Input, OutPer1M: p.Output})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": out})
}

// --- /admin/chaos ---------------------------------------------------------------

func (s *Server) adminOK(r *http.Request) bool {
	return s.cfg.AdminKey != "" && r.Header.Get("x-admin-key") == s.cfg.AdminKey
}

func (s *Server) handleChaos(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeError(w, 401, "unauthorized", "x-admin-key required")
		return
	}
	var body struct {
		Model   string `json:"model"`
		Mode    string `json:"mode"` // error | latency | clear
		Seconds int    `json:"seconds"`
		DelayMS int    `json:"delay_ms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	switch body.Mode {
	case "clear":
		s.faults.Clear(body.Model)
	case "error", "latency":
		if body.Seconds <= 0 {
			body.Seconds = 60
		}
		if body.Mode == "latency" && body.DelayMS <= 0 {
			body.DelayMS = 3000
		}
		s.faults.Set(Fault{Model: body.Model, Mode: body.Mode, DelayMS: body.DelayMS, Until: time.Now().Add(time.Duration(body.Seconds) * time.Second)})
	default:
		writeError(w, 400, "invalid_request", "mode must be error, latency or clear")
		return
	}
	slog.Warn("chaos fault updated", "model", body.Model, "mode", body.Mode, "seconds", body.Seconds)
	writeJSON(w, 200, map[string]any{"faults": s.faults.List()})
}

func (s *Server) handleChaosList(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeError(w, 401, "unauthorized", "x-admin-key required")
		return
	}
	writeJSON(w, 200, map[string]any{"faults": s.faults.List()})
}

// --- /v1/chat/completions -------------------------------------------------------

type requestState struct {
	id      string
	tenant  *Tenant
	req     *ChatRequest
	start   time.Time
	served  string
	outcome string
	ns      string
	key     string
	vec     []float32
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	span := trace.SpanFromContext(ctx)
	st := &requestState{id: newID("chatcmpl-"), start: time.Now(), outcome: "internal_error"}
	w.Header().Set("x-request-id", st.id)
	inflight.Inc()
	defer inflight.Dec()

	tenant, ok := s.authenticate(r)
	if !ok {
		requests.WithLabelValues("anonymous", "-", "unauthorized").Inc()
		writeError(w, 401, "invalid_api_key", "missing or invalid API key")
		return
	}
	st.tenant = tenant
	span.SetAttributes(attribute.String("llm.tenant", tenant.Name))

	var req ChatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		requests.WithLabelValues(tenant.Name, "-", "client_error").Inc()
		writeError(w, 400, "invalid_request", "bad JSON: "+err.Error())
		return
	}
	if req.Model == "" {
		req.Model = s.cfg.DefaultModel
	}
	st.req, st.served = &req, req.Model
	greq, err := toGemini(&req, s.cfg.ThinkingLevel)
	if err != nil {
		requests.WithLabelValues(tenant.Name, req.Model, "client_error").Inc()
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	chain := s.router.Chain(req.Model, tenant)
	if len(chain) == 0 {
		requests.WithLabelValues(tenant.Name, req.Model, "client_error").Inc()
		writeError(w, 403, "model_not_allowed", "model not available for this tenant: "+req.Model)
		return
	}
	span.SetAttributes(attribute.String("gen_ai.request.model", req.Model), attribute.Bool("llm.stream", req.Stream))

	defer func() {
		requests.WithLabelValues(tenant.Name, st.served, st.outcome).Inc()
		duration.WithLabelValues(st.served, strconv.FormatBool(req.Stream)).Observe(time.Since(st.start).Seconds())
		slog.Info("chat", append([]any{"id", st.id, "tenant", tenant.Name, "model", req.Model, "served", st.served,
			"outcome", st.outcome, "stream", req.Stream, "ms", time.Since(st.start).Milliseconds()}, logCtx(ctx, s.cfg.Project)...)...)
	}()

	// 1. tenant limits
	allowed, remaining, retry, _ := s.lim.Allow(ctx, tenant.Name, tenant.RPM, tenant.burst())
	w.Header().Set("x-ratelimit-limit-requests", strconv.Itoa(tenant.RPM))
	w.Header().Set("x-ratelimit-remaining-requests", strconv.Itoa(remaining))
	if !allowed {
		st.outcome = "rate_limited"
		rateLimited.WithLabelValues(tenant.Name, "rpm").Inc()
		w.Header().Set("Retry-After", strconv.Itoa(max(retry, 1)))
		writeError(w, 429, "rate_limit_exceeded", fmt.Sprintf("tenant %s exceeded %d requests/minute", tenant.Name, tenant.RPM))
		return
	}
	if tenant.DailyTokens > 0 {
		used, _ := s.lim.BudgetUsed(ctx, tenant.Name)
		w.Header().Set("x-budget-limit-tokens", strconv.FormatInt(tenant.DailyTokens, 10))
		w.Header().Set("x-budget-used-tokens", strconv.FormatInt(used, 10))
		if used >= tenant.DailyTokens {
			st.outcome = "budget_exhausted"
			rateLimited.WithLabelValues(tenant.Name, "daily_tokens").Inc()
			writeError(w, 429, "insufficient_quota", fmt.Sprintf("tenant %s used %d of %d tokens today (UTC)", tenant.Name, used, tenant.DailyTokens))
			return
		}
	}

	// 2. cache (exact, then semantic)
	st.ns = tenant.Name + "|" + req.Model
	st.key = cacheKey(st.ns, &req)
	if cached, err := s.cache.GetExact(ctx, st.ns, st.key); err != nil {
		cacheEvents.WithLabelValues("exact", "error").Inc()
	} else if cached != nil {
		cacheEvents.WithLabelValues("exact", "hit").Inc()
		s.respondCached(w, st, cached, "exact", 1)
		return
	} else {
		cacheEvents.WithLabelValues("exact", "miss").Inc()
	}
	if s.cfg.SemanticEnabled {
		if text := lastUserText(&req); text != "" {
			if vec, err := s.up.Embed(ctx, text); err != nil {
				cacheEvents.WithLabelValues("semantic", "error").Inc()
			} else {
				st.vec = vec
				cached, score, err := s.cache.GetSemantic(ctx, st.ns, vec)
				if score >= 0 {
					cacheScore.Observe(score)
				}
				switch {
				case err != nil:
					cacheEvents.WithLabelValues("semantic", "error").Inc()
				case cached != nil:
					cacheEvents.WithLabelValues("semantic", "hit").Inc()
					s.respondCached(w, st, cached, "semantic", score)
					return
				default:
					cacheEvents.WithLabelValues("semantic", "miss").Inc()
				}
			}
		}
	}

	// 3. upstream with breaker + fallback
	var lastErr error
	for i, m := range chain {
		if !s.router.Allow(m) {
			lastErr = fmt.Errorf("breaker open for %s", m)
			continue
		}
		if i > 0 {
			fallbacks.WithLabelValues(chain[0], m).Inc()
			span.AddEvent("fallback", trace.WithAttributes(attribute.String("to", m)))
		}
		st.served = m
		var committed bool
		if req.Stream {
			committed, err = s.streamUpstream(ctx, w, st, m, greq)
		} else {
			err = s.generateUpstream(ctx, w, st, m, greq)
		}
		if err == nil {
			s.router.Record(m, true)
			if i > 0 {
				st.outcome = "fallback_ok"
			} else {
				st.outcome = "ok"
			}
			return
		}
		var ue *UpstreamError
		retryable := errors.As(err, &ue) && ue.Retryable() || errors.Is(err, context.DeadlineExceeded)
		// A 4xx from the provider (bad request, safety) is not a provider health signal: only
		// retryable failures (429, 5xx, timeouts) count against the breaker.
		s.router.Record(m, !retryable)
		lastErr = err
		if committed {
			st.outcome = "upstream_error" // bytes already sent; nothing more we can do
			return
		}
		if !retryable {
			break
		}
	}
	st.outcome = "upstream_error"
	var ue *UpstreamError
	if errors.As(lastErr, &ue) && ue.Status >= 400 && ue.Status < 500 && ue.Status != 429 {
		writeError(w, ue.Status, "upstream_rejected", ue.Body)
		return
	}
	slog.Error("upstream failed", append([]any{"id", st.id, "err", lastErr}, logCtx(ctx, s.cfg.Project)...)...)
	writeError(w, 503, "upstream_unavailable", "all configured models failed: "+truncate(fmt.Sprint(lastErr), 200))
}

// record metrics, budget and cache after a successful upstream answer.
func (s *Server) finish(ctx context.Context, st *requestState, model, text, finish string, u *Usage) {
	cu := usageFrom(u)
	tokens.WithLabelValues(st.tenant.Name, model, "prompt").Add(float64(cu.PromptTokens))
	tokens.WithLabelValues(st.tenant.Name, model, "completion").Add(float64(cu.CompletionTokens))
	costUSD.WithLabelValues(st.tenant.Name, model).Add(s.prices.Cost(model, cu.PromptTokens, cu.CompletionTokens))
	s.lim.BudgetAdd(ctx, st.tenant.Name, int64(cu.TotalTokens))
	// Only complete, deterministic-ish answers are worth caching.
	cacheable := finish == "STOP" && strings.TrimSpace(text) != "" && (st.req.Temperature == nil || *st.req.Temperature <= 0.3)
	if cacheable {
		cr := &CachedResponse{Text: text, Model: model, Usage: cu, FinishReason: openAIFinish(finish), CreatedAt: nowUnix()}
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		if err := s.cache.PutExact(bg, st.ns, st.key, cr); err != nil {
			cacheEvents.WithLabelValues("exact", "error").Inc()
		}
		if st.vec != nil {
			if err := s.cache.PutSemantic(bg, st.ns, st.key[:24], st.vec, cr); err != nil {
				cacheEvents.WithLabelValues("semantic", "error").Inc()
			}
		}
	}
}

func (s *Server) generateUpstream(ctx context.Context, w http.ResponseWriter, st *requestState, model string, greq *GeminiRequest) error {
	resp, err := s.up.Generate(ctx, model, greq)
	if err != nil {
		return err
	}
	text, finish := resp.Text(), resp.FinishReason()
	s.finish(ctx, st, model, text, finish, resp.UsageMetadata)
	cu := usageFrom(resp.UsageMetadata)
	fr := openAIFinish(finish)
	w.Header().Set("x-cache", "miss")
	w.Header().Set("x-model-served", model)
	writeJSON(w, 200, ChatResponse{
		ID: st.id, Object: "chat.completion", Created: nowUnix(), Model: model,
		Choices: []Choice{{Index: 0, Message: &ChatMessage{Role: "assistant", Content: textContent(text)}, FinishReason: &fr}},
		Usage:   &cu,
	})
	return nil
}

// streamUpstream relays SSE chunks in the OpenAI delta format. It returns
// committed=true once any byte has been written: from then on a fallback is no
// longer possible and an error is reported inside the stream.
func (s *Server) streamUpstream(ctx context.Context, w http.ResponseWriter, st *requestState, model string, greq *GeminiRequest) (committed bool, err error) {
	flusher, _ := w.(http.Flusher)
	var text strings.Builder
	var usage *Usage
	finish := ""
	first := true
	writeChunk := func(delta string, fr *string, u *ChatUsage) {
		c := ChatResponse{ID: st.id, Object: "chat.completion.chunk", Created: nowUnix(), Model: model,
			Choices: []Choice{{Index: 0, Delta: &ChatMessage{}, FinishReason: fr}}}
		if delta != "" {
			c.Choices[0].Delta = &ChatMessage{Role: "assistant", Content: textContent(delta)}
		}
		if u != nil {
			c.Usage = u
		}
		b, _ := json.Marshal(c)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	err = s.up.Stream(ctx, model, greq, func(chunk *GeminiResponse) error {
		if first {
			first = false
			committed = true
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("x-cache", "miss")
			w.Header().Set("x-model-served", model)
			w.WriteHeader(200)
			ttft.WithLabelValues(model).Observe(time.Since(st.start).Seconds())
		}
		if t := chunk.Text(); t != "" {
			text.WriteString(t)
			writeChunk(t, nil, nil)
		}
		if chunk.UsageMetadata != nil && chunk.UsageMetadata.TotalTokenCount > 0 {
			usage = chunk.UsageMetadata
		}
		if f := chunk.FinishReason(); f != "" {
			finish = f
		}
		return nil
	})
	if err != nil {
		if committed {
			fmt.Fprintf(w, "data: {\"error\":{\"message\":%q,\"type\":\"upstream_error\"}}\n\ndata: [DONE]\n\n", truncate(err.Error(), 200))
			if flusher != nil {
				flusher.Flush()
			}
		}
		return committed, err
	}
	if !committed { // provider ended the stream without a single chunk
		return false, &UpstreamError{Status: 502, Body: "empty stream"}
	}
	cu := usageFrom(usage)
	fr := openAIFinish(finish)
	writeChunk("", &fr, &cu)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	s.finish(ctx, st, model, text.String(), finish, usage)
	return true, nil
}

func (s *Server) respondCached(w http.ResponseWriter, st *requestState, c *CachedResponse, layer string, score float64) {
	st.outcome = "cache_hit"
	st.served = c.Model
	w.Header().Set("x-cache", layer)
	w.Header().Set("x-cache-score", strconv.FormatFloat(score, 'f', 4, 64))
	w.Header().Set("x-model-served", c.Model)
	fr := c.FinishReason
	if st.req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		ttft.WithLabelValues(c.Model).Observe(time.Since(st.start).Seconds())
		chunk := ChatResponse{ID: st.id, Object: "chat.completion.chunk", Created: nowUnix(), Model: c.Model,
			Choices: []Choice{{Index: 0, Delta: &ChatMessage{Role: "assistant", Content: textContent(c.Text)}}}}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		chunk.Choices[0].Delta = &ChatMessage{}
		chunk.Choices[0].FinishReason = &fr
		u := c.Usage
		chunk.Usage = &u
		b, _ = json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
	u := c.Usage
	writeJSON(w, 200, ChatResponse{
		ID: st.id, Object: "chat.completion", Created: nowUnix(), Model: c.Model,
		Choices: []Choice{{Index: 0, Message: &ChatMessage{Role: "assistant", Content: textContent(c.Text)}, FinishReason: &fr}},
		Usage:   &u,
	})
}
