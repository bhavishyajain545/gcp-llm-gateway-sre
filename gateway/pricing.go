package main

import (
	"encoding/json"
	"log/slog"
)

// Price is USD per 1M tokens. The defaults are the on-demand "Global … Text
// Input/Output - Predictions" list prices read from the Cloud Billing catalog
// (service C7E2-9256-1C43) on 2026-11-22; override with PRICES_JSON when they
// change or when a promotion applies (Gemini 3.8 Flash is billed at half list
// price, 0.75 / 3.75, through 2026-12-31). Thinking tokens bill as output.
type Price struct {
	Input  float64 `json:"input_per_1m"`
	Output float64 `json:"output_per_1m"`
}

type Prices map[string]Price

var defaultPrices = Prices{
	"gemini-3.8-flash":      {Input: 1.50, Output: 7.50},
	"gemini-3.5-flash-lite": {Input: 0.30, Output: 2.50},
	"gemini-3.1-flash-lite": {Input: 0.25, Output: 1.50},
}

func loadPrices(js string) Prices {
	p := Prices{}
	for k, v := range defaultPrices {
		p[k] = v
	}
	if js == "" {
		return p
	}
	var override Prices
	if err := json.Unmarshal([]byte(js), &override); err != nil {
		slog.Error("PRICES_JSON ignored", "err", err)
		return p
	}
	for k, v := range override {
		p[k] = v
	}
	return p
}

// Cost in USD for one request.
func (p Prices) Cost(model string, promptTokens, outputTokens int) float64 {
	pr, ok := p[model]
	if !ok {
		return 0
	}
	return pr.Input*float64(promptTokens)/1e6 + pr.Output*float64(outputTokens)/1e6
}
