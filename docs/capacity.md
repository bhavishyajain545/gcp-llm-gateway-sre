# Capacity and cost model

How many gateway pods and how much Gemini spend a given traffic level needs, derived from the k6 run in
`loadtest/` and the Billing-catalog list prices. The numbers in §4 are from one afternoon on the demo cluster;
the method is the point.

## 1. What limits a gateway pod

A streamed request occupies a goroutine and an open connection to Vertex AI for the whole response: roughly
TTFT + output_tokens / tokens_per_second. CPU per request is small (JSON, SHA-256, one embedding call on
exact-cache misses). So the capacity of a pod is **concurrency**, not CPU, which is why the HPA scales on
`llm_inflight_requests` per pod and not on utilization.

Little's law gives the in-flight count for a request rate λ and a mean holding time W:

```
inflight = λ × W
```

With a per-pod target of `T` in-flight requests (the HPA's `averageValue`, 4 here):

```
replicas = ceil(λ × W / T)
```

`W` depends on the mix: cache hits hold for ~10 ms, cold streams for seconds. With hit rate `h`:

```
W = h × W_hit + (1 − h) × W_cold
```

## 2. Cost per request

Gemini bills input and output tokens (thinking tokens count as output); the embedding call bills input tokens
only on exact-cache misses (the semantic lookup needs the vector). With list prices `P_in`, `P_out` (USD per 1M)
and `P_emb` for the embedding model:

```
cost_cold  = P_in × in_tokens/1e6 + P_out × out_tokens/1e6 + P_emb × prompt_tokens/1e6
cost_hit   = P_emb × prompt_tokens/1e6        (semantic hit: embedding only)   or 0 (exact hit)
cost(h)    = h × cost_hit + (1 − h) × cost_cold
```

List prices on 2026-11-22 (Billing catalog, service `C7E2-9256-1C43`, "Global Text … - Predictions"):

| Model | Input USD/1M | Output USD/1M |
|---|---|---|
| gemini-3.8-flash | 1.50 (0.75 promotional through 2026-12-31) | 7.50 (3.75 promotional) |
| gemini-3.5-flash-lite | 0.30 | 2.50 |
| gemini-embedding-001 | 0.15 (text embedding SKU) | – |

For the measured prompt mix (system prompt + short question ≈ 40 input tokens, `max_tokens: 120`, ≈ 62 output
tokens including thinking), one cold request on 3.8-flash costs about **$0.00053** at list price; the same
request on flash-lite about **$0.00017**; a semantic hit about **$0.000006**; an exact hit $0.

So per 1,000 requests, list price, 3.8-flash:

| cache hit rate h | USD / 1k requests |
|---|---|
| 0% | 0.53 |
| 33% (measured) | 0.36 |
| 50% | 0.27 |
| 70% | 0.16 |

The fallback changes the mix, not the formula: when the breaker is open, requests move to the flash-lite row.

## 3. Platform cost (fixed, per day, us-central1, November 2026 list)

| Item | Approx. per day |
|---|---|
| 1 × e2-standard-2 on-demand (system pool) | ₹140 |
| 1-3 × e2-standard-2 Spot (gateway pool) | ₹40-120 |
| Memorystore Basic 1 GB | ₹100 |
| Global external ALB (forwarding rule + data) | ₹50 |
| Cloud NAT (gateway + data) | ₹90 |
| Cloud Armor policy + 4 rules (monthly, pro-rated) | ₹25 |
| GKE zonal cluster management | ₹0 (free tier) |
| **Total** | **≈ ₹450-525** |

The platform cost is flat; the model cost scales with traffic × (1 − h). Past roughly 15k cold requests a day
on 3.8-flash the model bill exceeds the platform bill, which is the point at which cache hit rate and the
cheaper fallback start to matter more than node sizing.

## 4. Measured (k6 run, see `loadtest/k6.js`)

Run of 2026-11-22 (`DURATION=150s PEAK_VUS=24`, 2 → 5 replicas, 1 → 2 Spot nodes), after the k6 profile was fixed
to back off on 429 and the platform tenant was sized at 3000 rpm:

| Metric | Value |
|---|---|
| Requests | 3,501 in 150 s (23.3 req/s mean; unique peak ≈ 10 req/s, warm ≈ 7 req/s) |
| Gateway / provider errors | 0 |
| Rate-limited | 240 (6.9%), all from `team-burst` (30 rpm, burst 10); `team-platform` 0% |
| Cache hits | 1,170 (33% overall; ~99% inside the warm scenario; the unique scenario also produced semantic hits for near-duplicate prompts) |
| TTFT cold (streamed, no cache) | p50 88 ms*, p95 **2.04 s**, p99 2.94 s |
| TTFT warm (cache hit) | p50 2.7 ms, p95 87 ms |
| Completion tokens | 145,045 (≈ 62 per cold request at `max_tokens: 120`, thinking LOW) |
| HPA | 2 → 5 replicas on `llm_inflight_requests` (target 4/pod); cluster autoscaler added one Spot node |
| CPU at peak | 50-500 m per pod (requests 250 m), memory < 20 MiB |

\* the p50 is low because near-duplicate prompts in the unique scenario hit the semantic cache; p95/p99 are the provider.

Reading the numbers:

- **Replica count check.** At the peak of the `unique` scenario, λ ≈ 10 req/s of cold streams with W ≈ 2.0 s
  gives in-flight ≈ 20 (the ~7 req/s of cache hits add ≈ 0.1); with T = 4 that is 5 replicas, which is what the HPA reached.
- **Where TTFT goes.** Cold TTFT is dominated by the provider's first-token latency (the `vertex.streamGenerateContent`
  span's `first_token` event); the gateway adds the embedding call (~200 ms including the Redis scan, measured as the difference between an exact and a semantic hit through the LB) on exact-cache misses and
  single-digit milliseconds otherwise. Warm TTFT is one Redis round trip.
- **The SLO moved.** The first TTFT objective (95% under 2 s) burned its budget within the hour: cold p95 is
  2.04 s and nothing in the gateway can change the provider's first-token time. The objective is now 3 s
  (p99 2.9 s), and 2 s is the stretch goal that only a higher cache hit rate or flash-lite can buy.
- **What would change in production.** Memorystore `STANDARD_HA` instead of Basic; `TRACE_RATIO` 0.05-0.2;
  a regional cluster; two Spot pools in different zones; and a per-tenant concurrency cap in addition to RPM,
  because concurrency, not request rate, is what the pod and the provider quota actually run out of.
