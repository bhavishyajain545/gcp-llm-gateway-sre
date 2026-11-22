package main

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Fault is an operator-requested failure for one model, used to rehearse
// fallback and breaker behaviour in a controlled way (see POST /admin/chaos).
// Faults are stored in Redis with a TTL so every replica applies them at once;
// each pod caches the lookup for a second. Without Redis they are per process.
type Fault struct {
	Model   string    `json:"model"`
	Mode    string    `json:"mode"` // "error" (503 before the call) or "latency" (sleep delay_ms first)
	DelayMS int       `json:"delay_ms,omitempty"`
	Until   time.Time `json:"until"`
}

type FaultInjector struct {
	rdb    *redis.Client
	mu     sync.RWMutex
	local  map[string]Fault     // authoritative when rdb == nil; a 1 s cache otherwise
	cached map[string]time.Time // model -> when `local` was last refreshed from Redis
}

const faultCacheTTL = time.Second

func NewFaultInjector(rdb *redis.Client) *FaultInjector {
	return &FaultInjector{rdb: rdb, local: map[string]Fault{}, cached: map[string]time.Time{}}
}

func faultKey(model string) string { return "fault:" + model }

func (f *FaultInjector) Set(fault Fault) {
	f.mu.Lock()
	f.local[fault.Model] = fault
	f.cached[fault.Model] = time.Now()
	f.mu.Unlock()
	if f.rdb != nil {
		b, _ := json.Marshal(fault)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = f.rdb.Set(ctx, faultKey(fault.Model), b, time.Until(fault.Until)).Err()
	}
}

func (f *FaultInjector) Clear(model string) {
	f.mu.Lock()
	if model == "" {
		f.local = map[string]Fault{}
		f.cached = map[string]time.Time{}
	} else {
		delete(f.local, model)
		f.cached[model] = time.Now()
	}
	f.mu.Unlock()
	if f.rdb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if model != "" {
		_ = f.rdb.Del(ctx, faultKey(model)).Err()
		return
	}
	iter := f.rdb.Scan(ctx, 0, "fault:*", 100).Iterator()
	for iter.Next(ctx) {
		_ = f.rdb.Del(ctx, iter.Val()).Err()
	}
}

func (f *FaultInjector) Get(model string) (Fault, bool) {
	f.mu.RLock()
	fl, ok := f.local[model]
	fresh := time.Since(f.cached[model]) < faultCacheTTL
	f.mu.RUnlock()
	if f.rdb != nil && !fresh {
		fl, ok = f.refresh(model)
	}
	if !ok || time.Now().After(fl.Until) {
		return Fault{}, false
	}
	return fl, true
}

func (f *FaultInjector) refresh(model string) (Fault, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	b, err := f.rdb.Get(ctx, faultKey(model)).Bytes()
	var fl Fault
	ok := err == nil && json.Unmarshal(b, &fl) == nil
	f.mu.Lock()
	if ok {
		f.local[model] = fl
	} else {
		delete(f.local, model)
	}
	f.cached[model] = time.Now()
	f.mu.Unlock()
	return fl, ok
}

func (f *FaultInjector) List() []Fault {
	now := time.Now()
	out := []Fault{}
	if f.rdb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		iter := f.rdb.Scan(ctx, 0, "fault:*", 100).Iterator()
		for iter.Next(ctx) {
			var fl Fault
			if b, err := f.rdb.Get(ctx, iter.Val()).Bytes(); err == nil && json.Unmarshal(b, &fl) == nil && now.Before(fl.Until) {
				out = append(out, fl)
			}
		}
		return out
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, fl := range f.local {
		if now.Before(fl.Until) {
			out = append(out, fl)
		}
	}
	return out
}
