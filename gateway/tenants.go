package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Tenant is one API consumer. Keys live in Secret Manager (never in Git or the
// container image); the gateway only keeps a SHA-256 of each key in memory.
type Tenant struct {
	Name        string   `json:"name"`
	Key         string   `json:"key"`
	RPM         int      `json:"rpm"`          // requests per minute (token bucket refill)
	Burst       int      `json:"burst"`        // bucket size; defaults to RPM/6 (10 s of traffic), min 1
	DailyTokens int64    `json:"daily_tokens"` // prompt+completion tokens per UTC day; 0 = unlimited
	Models      []string `json:"models"`       // allowed models; empty = all configured models
}

func (t *Tenant) Allows(model string) bool {
	if len(t.Models) == 0 {
		return true
	}
	for _, m := range t.Models {
		if m == model {
			return true
		}
	}
	return false
}

func (t *Tenant) burst() int {
	if t.Burst > 0 {
		return t.Burst
	}
	if b := t.RPM / 6; b > 0 {
		return b
	}
	return 1
}

type TenantStore struct {
	mu     sync.RWMutex
	byHash map[string]*Tenant
	loaded atomic.Bool
}

func NewTenantStore() *TenantStore { return &TenantStore{byHash: map[string]*Tenant{}} }

func hashKey(k string) string {
	h := sha256.Sum256([]byte(k))
	return hex.EncodeToString(h[:])
}

func (s *TenantStore) Replace(ts []Tenant) {
	m := make(map[string]*Tenant, len(ts))
	for i := range ts {
		t := ts[i]
		if t.Key == "" || t.Name == "" {
			continue
		}
		if t.RPM <= 0 {
			t.RPM = 60
		}
		m[hashKey(t.Key)] = &t
		t.Key = "" // drop the plaintext key; only the hash is needed from here on
	}
	s.mu.Lock()
	s.byHash = m
	s.mu.Unlock()
	s.loaded.Store(true)
}

func (s *TenantStore) Lookup(key string) (*Tenant, bool) {
	if key == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.byHash[hashKey(key)]
	return t, ok
}

func (s *TenantStore) Loaded() bool { return s.loaded.Load() }

func (s *TenantStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byHash)
}

func parseTenants(data []byte) ([]Tenant, error) {
	var ts []Tenant
	if err := json.Unmarshal(data, &ts); err != nil {
		return nil, fmt.Errorf("tenants json: %w", err)
	}
	return ts, nil
}

// SecretReader reads Secret Manager versions over REST with the ambient
// credentials (Workload Identity on GKE, ADC locally). No client library needed.
type SecretReader struct{ http *http.Client }

func (r *SecretReader) Access(ctx context.Context, name string) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://secretmanager.googleapis.com/v1/"+name+":access", nil)
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("secret %s: HTTP %d: %s", name, resp.StatusCode, truncate(string(body), 200))
	}
	var out struct {
		Payload struct {
			Data string `json:"data"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(out.Payload.Data)
}

// runTenantRefresh reloads the tenant list every `every`, so a key rotation in
// Secret Manager reaches every pod without a restart. A failed refresh keeps
// the last good set (and is counted).
func runTenantRefresh(ctx context.Context, store *TenantStore, reader *SecretReader, name string, every time.Duration) {
	load := func() {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		data, err := reader.Access(cctx, name)
		if err == nil {
			var ts []Tenant
			if ts, err = parseTenants(data); err == nil {
				store.Replace(ts)
				tenantRefresh.WithLabelValues("ok").Inc()
				return
			}
		}
		tenantRefresh.WithLabelValues("error").Inc()
		slog.Error("tenant refresh failed", "err", err)
	}
	load()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			load()
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
