package main

import (
	"sync"
	"time"
)

// Breaker is a small sliding-window circuit breaker per model: after
// `threshold` retryable failures within `window` it opens for `openFor`, during
// which requests go straight to the fallback model instead of waiting on a
// provider that is already failing. One request is let through when the open
// period ends (half-open); success closes the breaker, failure re-opens it.
type Breaker struct {
	mu        sync.Mutex
	failures  []time.Time
	openUntil time.Time
	halfOpen  bool
	probeAt   time.Time
	threshold int
	window    time.Duration
	openFor   time.Duration
}

func (b *Breaker) Allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now.Before(b.openUntil) {
		return false
	}
	if !b.openUntil.IsZero() && !b.halfOpen {
		b.halfOpen = true // first probe after the open period
		b.probeAt = now
		return true
	}
	if b.halfOpen {
		if now.Sub(b.probeAt) > b.openFor { // the probe never reported back; allow another one
			b.probeAt = now
			return true
		}
		return false // a probe is in flight; keep others on the fallback
	}
	return true
}

func (b *Breaker) Record(now time.Time, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ok {
		b.failures = b.failures[:0]
		b.openUntil = time.Time{}
		b.halfOpen = false
		return
	}
	if b.halfOpen {
		b.halfOpen = false
		b.openUntil = now.Add(b.openFor)
		return
	}
	cut := now.Add(-b.window)
	kept := b.failures[:0]
	for _, t := range b.failures {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	b.failures = append(kept, now)
	if len(b.failures) >= b.threshold {
		b.openUntil = now.Add(b.openFor)
		b.failures = b.failures[:0]
	}
}

func (b *Breaker) Open(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return now.Before(b.openUntil)
}

// Router decides the ordered list of models to try for a request.
type Router struct {
	cfg      Config
	mu       sync.Mutex
	breakers map[string]*Breaker
}

func NewRouter(cfg Config) *Router {
	return &Router{cfg: cfg, breakers: map[string]*Breaker{}}
}

func (r *Router) breaker(model string) *Breaker {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.breakers[model]
	if !ok {
		b = &Breaker{threshold: r.cfg.BreakerFailures, window: r.cfg.BreakerWindow, openFor: r.cfg.BreakerOpen}
		r.breakers[model] = b
	}
	return b
}

// Chain returns [requested, fallback, fallback-of-fallback…] restricted to the
// models the tenant may use and the models the gateway is configured with. A
// requested model the tenant may not use yields an empty chain (403), never a
// silent downgrade to a model they are allowed.
func (r *Router) Chain(model string, t *Tenant) []string {
	if !r.cfg.ModelKnown(model) || !t.Allows(model) {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for m := model; m != "" && !seen[m]; m = r.cfg.Fallbacks[m] {
		seen[m] = true
		if r.cfg.ModelKnown(m) && t.Allows(m) {
			out = append(out, m)
		}
	}
	return out
}

func (r *Router) Allow(model string) bool {
	ok := r.breaker(model).Allow(time.Now())
	if !ok {
		breakerSkips.WithLabelValues(model).Inc()
	}
	return ok
}

func (r *Router) Record(model string, ok bool) {
	b := r.breaker(model)
	b.Record(time.Now(), ok)
	if b.Open(time.Now()) {
		breakerOpen.WithLabelValues(model).Set(1)
	} else {
		breakerOpen.WithLabelValues(model).Set(0)
	}
}
