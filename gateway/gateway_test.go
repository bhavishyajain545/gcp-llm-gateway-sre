package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// fakeUpstream stands in for Vertex AI: canned answers, per-model failures and
// canned embeddings so the semantic cache can be tested deterministically.
type fakeUpstream struct {
	mu    sync.Mutex
	fail  map[string]int // model -> HTTP status to fail with
	calls map[string]int
	reply string
}

func newFake() *fakeUpstream {
	return &fakeUpstream{fail: map[string]int{}, calls: map[string]int{}, reply: "An SLO is a reliability target."}
}

func (f *fakeUpstream) count(model string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[model]
}

func (f *fakeUpstream) check(model string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[model]++
	if st := f.fail[model]; st != 0 {
		return &UpstreamError{Status: st, Body: "boom"}
	}
	return nil
}

func (f *fakeUpstream) response(model string) *GeminiResponse {
	return &GeminiResponse{
		Candidates:    []Candidate{{Content: GContent{Role: "model", Parts: []GPart{{Text: f.reply}}}, FinishReason: "STOP"}},
		UsageMetadata: &Usage{PromptTokenCount: 10, CandidatesTokenCount: 5, TotalTokenCount: 15},
		ModelVersion:  model,
	}
}

func (f *fakeUpstream) Generate(_ context.Context, model string, _ *GeminiRequest) (*GeminiResponse, error) {
	if err := f.check(model); err != nil {
		return nil, err
	}
	return f.response(model), nil
}

func (f *fakeUpstream) Stream(_ context.Context, model string, _ *GeminiRequest, onChunk func(*GeminiResponse) error) error {
	if err := f.check(model); err != nil {
		return err
	}
	words := strings.Fields(f.reply)
	for i, w := range words {
		c := &GeminiResponse{Candidates: []Candidate{{Content: GContent{Role: "model", Parts: []GPart{{Text: w + " "}}}}}}
		if i == len(words)-1 {
			c.Candidates[0].FinishReason = "STOP"
			c.UsageMetadata = &Usage{PromptTokenCount: 10, CandidatesTokenCount: 5, TotalTokenCount: 15}
		}
		if err := onChunk(c); err != nil {
			return err
		}
	}
	return nil
}

// Embed: prompts about SLOs share one direction; everything else gets a vector
// derived from its text so unrelated prompts never collide.
func (f *fakeUpstream) Embed(_ context.Context, text string) ([]float32, error) {
	v := make([]float32, 8)
	if strings.Contains(strings.ToLower(text), "slo") {
		v[0] = 1
		return v, nil
	}
	sum := sha256.Sum256([]byte(text)) // unrelated prompts: pseudo-random directions
	for i := range v {
		v[i] = float32(sum[i]) - 128
	}
	return normalize(v), nil
}

const tenantsJSON = `[
  {"name":"team-a","key":"key-a","rpm":6000,"burst":100,"daily_tokens":0},
  {"name":"team-b","key":"key-b","rpm":60,"burst":1,"daily_tokens":0,"models":["gemini-3.5-flash-lite"]},
  {"name":"team-c","key":"key-c","rpm":6000,"burst":100,"daily_tokens":30}
]`

func newTestServer(t *testing.T) (*httptest.Server, *fakeUpstream, *Server) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	cfg := Config{
		Models: []string{"gemini-3.8-flash", "gemini-3.5-flash-lite"}, DefaultModel: "gemini-3.8-flash",
		Fallbacks: map[string]string{"gemini-3.8-flash": "gemini-3.5-flash-lite"}, ThinkingLevel: "LOW",
		CacheTTL: time.Hour, SemanticEnabled: true, SemanticThreshold: 0.95, SemanticMax: 100,
		BreakerFailures: 2, BreakerWindow: 30 * time.Second, BreakerOpen: 30 * time.Second,
		AdminKey: "admin-secret", Project: "test",
	}
	ts, err := parseTenants([]byte(tenantsJSON))
	if err != nil {
		t.Fatal(err)
	}
	store := NewTenantStore()
	store.Replace(ts)
	fake := newFake()
	srv := &Server{
		cfg: cfg, version: "test", up: fake,
		cache: NewCache(rdb, cfg.CacheTTL, cfg.SemanticThreshold, cfg.SemanticMax), lim: NewLimiter(rdb),
		router: NewRouter(cfg), tenants: store, faults: NewFaultInjector(rdb), prices: loadPrices(""), rdb: rdb,
	}
	hs := httptest.NewServer(srv.routes())
	t.Cleanup(hs.Close)
	return hs, fake, srv
}

func chat(t *testing.T, url, key, prompt, model string, stream bool) (*http.Response, string) {
	t.Helper()
	body := map[string]any{"messages": []map[string]string{{"role": "user", "content": prompt}}, "stream": stream}
	if model != "" {
		body["model"] = model
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, string(out)
}

func TestUnauthorized(t *testing.T) {
	hs, _, _ := newTestServer(t)
	resp, body := chat(t, hs.URL, "", "hi", "", false)
	if resp.StatusCode != 401 || !strings.Contains(body, "invalid_api_key") {
		t.Fatalf("want 401 invalid_api_key, got %d %s", resp.StatusCode, body)
	}
	resp, _ = chat(t, hs.URL, "wrong", "hi", "", false)
	if resp.StatusCode != 401 {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

func TestChatAndExactCache(t *testing.T) {
	hs, fake, _ := newTestServer(t)
	resp, body := chat(t, hs.URL, "key-a", "Explain error budgets", "", false)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if resp.Header.Get("x-cache") != "miss" || resp.Header.Get("x-model-served") != "gemini-3.8-flash" {
		t.Fatalf("headers: %v", resp.Header)
	}
	var cr ChatResponse
	if err := json.Unmarshal([]byte(body), &cr); err != nil || len(cr.Choices) != 1 || cr.Choices[0].Message.Text() != fake.reply {
		t.Fatalf("bad response: %s", body)
	}
	if cr.Usage == nil || cr.Usage.TotalTokens != 15 {
		t.Fatalf("usage: %+v", cr.Usage)
	}
	resp, _ = chat(t, hs.URL, "key-a", "Explain error budgets", "", false)
	if resp.Header.Get("x-cache") != "exact" {
		t.Fatalf("second call should hit the exact cache, got %q", resp.Header.Get("x-cache"))
	}
	if fake.count("gemini-3.8-flash") != 1 {
		t.Fatalf("upstream should be called once, got %d", fake.count("gemini-3.8-flash"))
	}
}

func TestSemanticCacheIsPerTenant(t *testing.T) {
	hs, fake, _ := newTestServer(t)
	resp, _ := chat(t, hs.URL, "key-a", "What is an SLO?", "", false)
	if resp.Header.Get("x-cache") != "miss" {
		t.Fatal("first request must miss")
	}
	resp, _ = chat(t, hs.URL, "key-a", "what's an slo, briefly", "", false)
	if resp.Header.Get("x-cache") != "semantic" {
		t.Fatalf("paraphrase should hit the semantic cache, got %q", resp.Header.Get("x-cache"))
	}
	if fake.count("gemini-3.8-flash") != 1 {
		t.Fatalf("upstream calls = %d, want 1", fake.count("gemini-3.8-flash"))
	}
	// Another tenant asking the same thing must not get team-a's cached answer.
	resp, _ = chat(t, hs.URL, "key-c", "What is an SLO?", "", false)
	if resp.Header.Get("x-cache") != "miss" {
		t.Fatalf("cache must be namespaced per tenant, got %q", resp.Header.Get("x-cache"))
	}
}

func TestRateLimit(t *testing.T) {
	hs, _, _ := newTestServer(t)
	resp, _ := chat(t, hs.URL, "key-b", "first", "gemini-3.5-flash-lite", false)
	if resp.StatusCode != 200 {
		t.Fatalf("first: %d", resp.StatusCode)
	}
	resp, body := chat(t, hs.URL, "key-b", "second", "gemini-3.5-flash-lite", false)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" || !strings.Contains(body, "rate_limit_exceeded") {
		t.Fatalf("want 429 with Retry-After, got %d %v %s", resp.StatusCode, resp.Header, body)
	}
}

func TestDailyTokenBudget(t *testing.T) {
	hs, _, _ := newTestServer(t)
	// each answer is 15 tokens; the budget is 30
	for i, want := range []int{200, 200, 429} {
		resp, body := chat(t, hs.URL, "key-c", fmt.Sprintf("prompt number %d about kubernetes", i), "", false)
		if resp.StatusCode != want {
			t.Fatalf("request %d: want %d got %d (%s)", i, want, resp.StatusCode, body)
		}
		if want == 429 && !strings.Contains(body, "insufficient_quota") {
			t.Fatalf("want insufficient_quota, got %s", body)
		}
	}
}

func TestModelNotAllowed(t *testing.T) {
	hs, _, _ := newTestServer(t)
	resp, body := chat(t, hs.URL, "key-b", "hi", "gemini-3.8-flash", false)
	if resp.StatusCode != 403 || !strings.Contains(body, "model_not_allowed") {
		t.Fatalf("want 403, got %d %s", resp.StatusCode, body)
	}
}

func TestFallbackAndBreaker(t *testing.T) {
	hs, fake, srv := newTestServer(t)
	fake.fail["gemini-3.8-flash"] = 503
	for i := 0; i < 3; i++ {
		resp, body := chat(t, hs.URL, "key-a", fmt.Sprintf("unique prompt %d about networking", i), "", false)
		if resp.StatusCode != 200 || resp.Header.Get("x-model-served") != "gemini-3.5-flash-lite" {
			t.Fatalf("request %d: want fallback to flash-lite, got %d %v %s", i, resp.StatusCode, resp.Header, body)
		}
	}
	// two failures open the breaker, so the third request never touched the primary
	if got := fake.count("gemini-3.8-flash"); got != 2 {
		t.Fatalf("primary calls = %d, want 2 (breaker should be open)", got)
	}
	if !srv.router.breaker("gemini-3.8-flash").Open(time.Now()) {
		t.Fatal("breaker should be open")
	}
	// a non-retryable 4xx must be returned, not retried on the fallback
	fake.fail["gemini-3.5-flash-lite"] = 400
	srv.router.breaker("gemini-3.8-flash").Record(time.Now(), true) // close it again
	fake.fail["gemini-3.8-flash"] = 400
	resp, body := chat(t, hs.URL, "key-a", "another unique prompt about storage", "", false)
	if resp.StatusCode != 400 || !strings.Contains(body, "upstream_rejected") {
		t.Fatalf("want 400 upstream_rejected, got %d %s", resp.StatusCode, body)
	}
}

func TestAllModelsDown(t *testing.T) {
	hs, fake, _ := newTestServer(t)
	fake.fail["gemini-3.8-flash"] = 503
	fake.fail["gemini-3.5-flash-lite"] = 429
	resp, body := chat(t, hs.URL, "key-a", "hello there", "", false)
	if resp.StatusCode != 503 || !strings.Contains(body, "upstream_unavailable") {
		t.Fatalf("want 503, got %d %s", resp.StatusCode, body)
	}
}

func TestStreaming(t *testing.T) {
	hs, fake, _ := newTestServer(t)
	resp, body := chat(t, hs.URL, "key-a", "stream me something about latency", "", true)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d type %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(body, "chat.completion.chunk") || !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("bad SSE body: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) || !strings.Contains(body, `"total_tokens":15`) {
		t.Fatalf("final chunk missing finish/usage: %s", body)
	}
	// streamed answers are cached too, and a cached stream still looks like a stream
	resp, body = chat(t, hs.URL, "key-a", "stream me something about latency", "", true)
	if resp.Header.Get("x-cache") != "exact" || !strings.Contains(body, "[DONE]") {
		t.Fatalf("cached stream: %v %s", resp.Header, body)
	}
	if fake.count("gemini-3.8-flash") != 1 {
		t.Fatalf("upstream calls = %d", fake.count("gemini-3.8-flash"))
	}
}

func TestChaosEndpoint(t *testing.T) {
	hs, _, srv := newTestServer(t)
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/admin/chaos", strings.NewReader(`{"model":"gemini-3.8-flash","mode":"error","seconds":30}`))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("no admin key: want 401, got %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodPost, hs.URL+"/admin/chaos", strings.NewReader(`{"model":"gemini-3.8-flash","mode":"error","seconds":30}`))
	req.Header.Set("x-admin-key", "admin-secret")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("set fault: %d", resp.StatusCode)
	}
	if f, ok := srv.faults.Get("gemini-3.8-flash"); !ok || f.Mode != "error" {
		t.Fatalf("fault not stored: %+v %v", f, ok)
	}
	// a second replica sharing the same Redis sees the fault too
	other := NewFaultInjector(srv.rdb)
	if f, ok := other.Get("gemini-3.8-flash"); !ok || f.Mode != "error" {
		t.Fatalf("fault not shared through Redis: %+v %v", f, ok)
	}
	if n := len(other.List()); n != 1 {
		t.Fatalf("list = %d faults, want 1", n)
	}
	req, _ = http.NewRequest(http.MethodPost, hs.URL+"/admin/chaos", strings.NewReader(`{"model":"gemini-3.8-flash","mode":"clear"}`))
	req.Header.Set("x-admin-key", "admin-secret")
	resp, _ = http.DefaultClient.Do(req)
	if _, ok := srv.faults.Get("gemini-3.8-flash"); ok || resp.StatusCode != 200 {
		t.Fatal("fault should be cleared")
	}
}

func TestModelsEndpoint(t *testing.T) {
	hs, _, _ := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, hs.URL+"/v1/models", nil)
	req.Header.Set("x-api-key", "key-b")
	resp, _ := http.DefaultClient.Do(req)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || strings.Contains(string(body), "gemini-3.8-flash\"") || !strings.Contains(string(body), "gemini-3.5-flash-lite") {
		t.Fatalf("models for team-b: %d %s", resp.StatusCode, body)
	}
}

func TestToGemini(t *testing.T) {
	temp := 0.1
	req := &ChatRequest{Model: "gemini-3.8-flash", Temperature: &temp, Messages: []ChatMessage{
		{Role: "system", Content: textContent("Be terse.")},
		{Role: "user", Content: textContent("Hi")},
		{Role: "assistant", Content: textContent("Hello.")},
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"Parts "},{"type":"text","text":"form"}]`)},
	}}
	g, err := toGemini(req, "LOW")
	if err != nil {
		t.Fatal(err)
	}
	if g.SystemInstruction == nil || g.SystemInstruction.Parts[0].Text != "Be terse." {
		t.Fatalf("system instruction: %+v", g.SystemInstruction)
	}
	if len(g.Contents) != 3 || g.Contents[1].Role != "model" || g.Contents[2].Parts[0].Text != "Parts form" {
		t.Fatalf("contents: %+v", g.Contents)
	}
	if g.GenerationConfig.ThinkingConfig == nil || g.GenerationConfig.ThinkingConfig.ThinkingLevel != "LOW" || *g.GenerationConfig.Temperature != 0.1 {
		t.Fatalf("generation config: %+v", g.GenerationConfig)
	}
	if _, err := toGemini(&ChatRequest{Messages: []ChatMessage{{Role: "system", Content: textContent("only system")}}}, ""); err == nil {
		t.Fatal("system-only request must be rejected")
	}
	k1 := cacheKey("a|m", req)
	req.Messages[3].Content = textContent("Parts form")
	if cacheKey("a|m", req) != k1 {
		t.Fatal("cache key must depend on text, not on the JSON shape")
	}
	if cacheKey("b|m", req) == k1 {
		t.Fatal("cache key must include the namespace")
	}
}

func TestBreakerHalfOpen(t *testing.T) {
	b := &Breaker{threshold: 2, window: time.Minute, openFor: 10 * time.Second}
	now := time.Now()
	b.Record(now, false)
	b.Record(now, false)
	if b.Allow(now.Add(time.Second)) {
		t.Fatal("should be open")
	}
	probe := now.Add(11 * time.Second)
	if !b.Allow(probe) {
		t.Fatal("first request after the open period is the probe")
	}
	if b.Allow(probe.Add(time.Millisecond)) {
		t.Fatal("while a probe is in flight others stay on the fallback")
	}
	b.Record(probe.Add(time.Second), false)
	if b.Allow(probe.Add(2 * time.Second)) {
		t.Fatal("failed probe re-opens")
	}
	later := probe.Add(12 * time.Second)
	if !b.Allow(later) {
		t.Fatal("probe again")
	}
	b.Record(later, true)
	if !b.Allow(later.Add(time.Millisecond)) || b.Open(later) {
		t.Fatal("successful probe closes the breaker")
	}
}

func TestPrices(t *testing.T) {
	p := loadPrices(`{"gemini-3.8-flash":{"input_per_1m":1,"output_per_1m":10}}`)
	if c := p.Cost("gemini-3.8-flash", 1_000_000, 100_000); c != 2 {
		t.Fatalf("cost = %v, want 2", c)
	}
	if p.Cost("unknown", 10, 10) != 0 {
		t.Fatal("unknown model costs 0")
	}
	if _, ok := p["gemini-3.5-flash-lite"]; !ok {
		t.Fatal("defaults must survive an override")
	}
}
