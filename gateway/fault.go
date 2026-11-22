package main

import (
	"sync"
	"time"
)

// Fault is an operator-requested failure for one model, used to rehearse
// fallback and breaker behaviour in a controlled way (see POST /admin/chaos).
// Faults are per pod and expire on their own; they are never persisted.
type Fault struct {
	Model   string    `json:"model"`
	Mode    string    `json:"mode"` // "error" (503 before the call) or "latency" (sleep delay_ms first)
	DelayMS int       `json:"delay_ms,omitempty"`
	Until   time.Time `json:"until"`
}

type FaultInjector struct {
	mu     sync.RWMutex
	faults map[string]Fault
}

func NewFaultInjector() *FaultInjector { return &FaultInjector{faults: map[string]Fault{}} }

func (f *FaultInjector) Set(fault Fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults[fault.Model] = fault
}

func (f *FaultInjector) Clear(model string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if model == "" {
		f.faults = map[string]Fault{}
		return
	}
	delete(f.faults, model)
}

func (f *FaultInjector) Get(model string) (Fault, bool) {
	f.mu.RLock()
	fl, ok := f.faults[model]
	f.mu.RUnlock()
	if !ok {
		return Fault{}, false
	}
	if time.Now().After(fl.Until) {
		f.Clear(model)
		return Fault{}, false
	}
	return fl, true
}

func (f *FaultInjector) List() []Fault {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]Fault, 0, len(f.faults))
	now := time.Now()
	for _, fl := range f.faults {
		if now.Before(fl.Until) {
			out = append(out, fl)
		}
	}
	return out
}
