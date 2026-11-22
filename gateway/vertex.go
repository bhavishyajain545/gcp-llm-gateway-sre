package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Upstream is what the gateway needs from a model provider. VertexClient is the
// real one; tests use a fake.
type Upstream interface {
	Generate(ctx context.Context, model string, req *GeminiRequest) (*GeminiResponse, error)
	Stream(ctx context.Context, model string, req *GeminiRequest, onChunk func(*GeminiResponse) error) error
	Embed(ctx context.Context, text string) ([]float32, error)
}

// --- Gemini wire types (only the fields the gateway uses) -------------------

type GeminiRequest struct {
	Contents          []GContent `json:"contents"`
	SystemInstruction *GContent  `json:"systemInstruction,omitempty"`
	GenerationConfig  *GenConfig `json:"generationConfig,omitempty"`
}

type GContent struct {
	Role  string  `json:"role,omitempty"`
	Parts []GPart `json:"parts"`
}

type GPart struct {
	Text string `json:"text,omitempty"`
}

type GenConfig struct {
	MaxOutputTokens int             `json:"maxOutputTokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"topP,omitempty"`
	ThinkingConfig  *ThinkingConfig `json:"thinkingConfig,omitempty"`
}

type ThinkingConfig struct {
	ThinkingLevel string `json:"thinkingLevel,omitempty"`
}

type GeminiResponse struct {
	Candidates    []Candidate `json:"candidates"`
	UsageMetadata *Usage      `json:"usageMetadata"`
	ModelVersion  string      `json:"modelVersion"`
}

type Candidate struct {
	Content      GContent `json:"content"`
	FinishReason string   `json:"finishReason"`
}

type Usage struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

func (r *GeminiResponse) Text() string {
	if r == nil || len(r.Candidates) == 0 {
		return ""
	}
	var b strings.Builder
	for _, p := range r.Candidates[0].Content.Parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

func (r *GeminiResponse) FinishReason() string {
	if r == nil || len(r.Candidates) == 0 {
		return ""
	}
	return r.Candidates[0].FinishReason
}

// UpstreamError carries the provider's HTTP status so the router can decide
// whether to fall back (429/5xx/timeouts) or return the error (4xx).
type UpstreamError struct {
	Status int
	Body   string
}

func (e *UpstreamError) Error() string { return fmt.Sprintf("upstream HTTP %d: %s", e.Status, e.Body) }

// Retryable reports whether another model should be tried.
func (e *UpstreamError) Retryable() bool {
	return e.Status == 429 || e.Status == 408 || e.Status >= 500
}

// --- Vertex AI client --------------------------------------------------------

type VertexClient struct {
	http          *http.Client
	project       string
	location      string
	embedLocation string
	embedModel    string
	embedDims     int
	timeout       time.Duration
	faults        *FaultInjector
}

func NewVertexClient(ctx context.Context, cfg Config, faults *FaultInjector) (*VertexClient, error) {
	ts, err := google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, fmt.Errorf("credentials: %w", err)
	}
	base := &http.Transport{MaxIdleConns: 100, MaxIdleConnsPerHost: 100, IdleConnTimeout: 90 * time.Second}
	hc := &http.Client{
		Transport: &oauth2.Transport{Source: oauth2.ReuseTokenSource(nil, ts), Base: otelhttp.NewTransport(base)},
	}
	return &VertexClient{
		http: hc, project: cfg.Project, location: cfg.Location,
		embedLocation: cfg.EmbedLocation, embedModel: cfg.EmbedModel, embedDims: cfg.EmbedDims,
		timeout: cfg.UpstreamTimeout, faults: faults,
	}, nil
}

// HTTPClient exposes the authenticated client for other Google APIs (Secret Manager).
func (c *VertexClient) HTTPClient() *http.Client { return c.http }

func hostFor(location string) string {
	if location == "global" {
		return "https://aiplatform.googleapis.com"
	}
	return "https://" + location + "-aiplatform.googleapis.com"
}

func (c *VertexClient) modelURL(model, method string) string {
	return fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/google/models/%s:%s",
		hostFor(c.location), c.project, c.location, model, method)
}

// injectFault applies an operator-set fault (see /admin/chaos) before a real call.
func (c *VertexClient) injectFault(ctx context.Context, model string) error {
	if c.faults == nil {
		return nil
	}
	f, ok := c.faults.Get(model)
	if !ok {
		return nil
	}
	faultsApplied.WithLabelValues(model, f.Mode).Inc()
	switch f.Mode {
	case "error":
		return &UpstreamError{Status: 503, Body: "fault injection: forced upstream error"}
	case "latency":
		select {
		case <-time.After(time.Duration(f.DelayMS) * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (c *VertexClient) Generate(ctx context.Context, model string, req *GeminiRequest) (*GeminiResponse, error) {
	ctx, span := tracer.Start(ctx, "vertex.generateContent")
	defer span.End()
	span.SetAttributes(attribute.String("gen_ai.request.model", model))
	if err := c.injectFault(ctx, model); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	body, _ := json.Marshal(req)
	start := time.Now()
	resp, err := c.post(ctx, c.modelURL(model, "generateContent"), body)
	if err != nil {
		upstreamRequests.WithLabelValues(model, "error").Inc()
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	defer resp.Body.Close()
	upstreamRequests.WithLabelValues(model, fmt.Sprint(resp.StatusCode)).Inc()
	upstreamLatency.WithLabelValues(model).Observe(time.Since(start).Seconds())
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		e := &UpstreamError{Status: resp.StatusCode, Body: truncate(string(b), 300)}
		span.SetStatus(codes.Error, e.Error())
		return nil, e
	}
	var out GeminiResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if out.UsageMetadata != nil {
		span.SetAttributes(
			attribute.Int("gen_ai.usage.input_tokens", out.UsageMetadata.PromptTokenCount),
			attribute.Int("gen_ai.usage.output_tokens", out.UsageMetadata.CandidatesTokenCount+out.UsageMetadata.ThoughtsTokenCount),
		)
	}
	return &out, nil
}

// Stream calls streamGenerateContent with alt=sse and hands every chunk to onChunk.
func (c *VertexClient) Stream(ctx context.Context, model string, req *GeminiRequest, onChunk func(*GeminiResponse) error) error {
	ctx, span := tracer.Start(ctx, "vertex.streamGenerateContent")
	defer span.End()
	span.SetAttributes(attribute.String("gen_ai.request.model", model))
	if err := c.injectFault(ctx, model); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	body, _ := json.Marshal(req)
	start := time.Now()
	resp, err := c.post(ctx, c.modelURL(model, "streamGenerateContent")+"?alt=sse", body)
	if err != nil {
		upstreamRequests.WithLabelValues(model, "error").Inc()
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	defer resp.Body.Close()
	upstreamRequests.WithLabelValues(model, fmt.Sprint(resp.StatusCode)).Inc()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		e := &UpstreamError{Status: resp.StatusCode, Body: truncate(string(b), 300)}
		span.SetStatus(codes.Error, e.Error())
		return e
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	first := true
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[5:])
		if len(payload) == 0 {
			continue
		}
		var chunk GeminiResponse
		if err := json.Unmarshal(payload, &chunk); err != nil {
			return fmt.Errorf("decode chunk: %w", err)
		}
		if first {
			first = false
			span.AddEvent("first_token")
			upstreamTTFT.WithLabelValues(model).Observe(time.Since(start).Seconds())
		}
		if err := onChunk(&chunk); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return &UpstreamError{Status: 502, Body: "stream read: " + err.Error()}
	}
	upstreamLatency.WithLabelValues(model).Observe(time.Since(start).Seconds())
	return nil
}

func (c *VertexClient) Embed(ctx context.Context, text string) ([]float32, error) {
	ctx, span := tracer.Start(ctx, "vertex.embed")
	defer span.End()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	url := fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/google/models/%s:predict",
		hostFor(c.embedLocation), c.project, c.embedLocation, c.embedModel)
	body, _ := json.Marshal(map[string]any{
		"instances":  []map[string]any{{"content": text, "task_type": "SEMANTIC_SIMILARITY"}},
		"parameters": map[string]any{"outputDimensionality": c.embedDims},
	})
	start := time.Now()
	resp, err := c.post(ctx, url, body)
	if err != nil {
		embedRequests.WithLabelValues("error").Inc()
		return nil, err
	}
	defer resp.Body.Close()
	embedRequests.WithLabelValues(fmt.Sprint(resp.StatusCode)).Inc()
	embedLatency.Observe(time.Since(start).Seconds())
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, &UpstreamError{Status: resp.StatusCode, Body: truncate(string(b), 200)}
	}
	var out struct {
		Predictions []struct {
			Embeddings struct {
				Values []float32 `json:"values"`
			} `json:"embeddings"`
		} `json:"predictions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Predictions) != 1 || len(out.Predictions[0].Embeddings.Values) == 0 {
		return nil, fmt.Errorf("embedding: unexpected response shape")
	}
	return normalize(out.Predictions[0].Embeddings.Values), nil
}

func (c *VertexClient) post(ctx context.Context, url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.http.Do(req)
}

// normalize scales a vector to unit length so cosine similarity is a dot product.
func normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	n := float32(math.Sqrt(sum))
	if n == 0 {
		return v
	}
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x / n
	}
	return out
}

func dot(a, b []float32) float64 {
	if len(a) != len(b) {
		return -1
	}
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}
