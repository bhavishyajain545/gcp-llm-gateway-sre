// k6 load profile for the LLM gateway. Three scenarios run together:
//   warm_cache   repeated prompts from a small pool  -> exact/semantic cache hits (team-platform)
//   unique       streaming, unique prompts, ramping  -> real Gemini traffic, TTFT measured as time-to-first-byte
//   burst_tenant a low-quota tenant hammering        -> 429s from the gateway's token bucket, not errors
// Thresholds encode the SLOs: TTFT p95 < 2 s on cold streams, < 0.5% gateway/provider errors.
import http from "k6/http";
import { check } from "k6";
import { Counter, Rate, Trend } from "k6/metrics";

const BASE = __ENV.GATEWAY_URL || "http://llm-gateway.llm-gateway.svc.cluster.local";
const KEY_PLATFORM = __ENV.KEY_PLATFORM;
const KEY_BURST = __ENV.KEY_BURST;
const DURATION = __ENV.DURATION || "120s";
const PEAK_VUS = Number(__ENV.PEAK_VUS || 24);

const ttftCold = new Trend("ttft_cold_ms", true);
const ttftWarm = new Trend("ttft_warm_ms", true);
const cacheHits = new Counter("cache_hits");
const rateLimited = new Rate("rate_limited");
const gatewayErrors = new Rate("gateway_errors");
const tokens = new Counter("completion_tokens");

export const options = {
  scenarios: {
    warm_cache: {
      executor: "constant-vus", vus: 4, duration: DURATION, exec: "warm",
      tags: { scenario: "warm_cache" },
    },
    unique: {
      executor: "ramping-vus", exec: "unique", tags: { scenario: "unique" },
      stages: [
        { duration: "30s", target: 4 },
        { duration: "45s", target: PEAK_VUS },
        { duration: "30s", target: PEAK_VUS },
        { duration: "15s", target: 0 },
      ],
    },
    burst_tenant: {
      executor: "constant-vus", vus: 6, duration: "40s", startTime: "20s", exec: "burst",
      tags: { scenario: "burst_tenant" },
    },
  },
  thresholds: {
    "ttft_cold_ms": ["p(95)<2000"],
    "gateway_errors": ["rate<0.005"],
    "http_req_failed{scenario:unique}": ["rate<0.01"],
  },
  summaryTrendStats: ["avg", "p(50)", "p(95)", "p(99)", "max"],
};

const topics = ["error budgets", "SLIs vs SLOs", "canary deployments", "circuit breakers", "Kubernetes HPA",
  "Cloud NAT", "Workload Identity", "token bucket rate limiting", "semantic caching", "p95 latency"];

function chat(key, prompt, stream, extraTags) {
  const body = JSON.stringify({
    model: "gemini-3.8-flash",
    messages: [
      { role: "system", content: "You are a concise SRE assistant. Answer in two sentences." },
      { role: "user", content: prompt },
    ],
    stream, max_tokens: 120, temperature: 0.1,
  });
  return http.post(`${BASE}/v1/chat/completions`, body, {
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${key}` },
    timeout: "60s", tags: extraTags,
  });
}

export function warm() {
  const topic = topics[Math.floor(Math.random() * topics.length)];
  const res = chat(KEY_PLATFORM, `Explain ${topic} briefly.`, true, { kind: "warm" });
  const hit = res.headers["X-Cache"] && res.headers["X-Cache"] !== "miss";
  if (hit) { cacheHits.add(1); ttftWarm.add(res.timings.waiting); } else { ttftCold.add(res.timings.waiting); }
  gatewayErrors.add(res.status >= 500);
  check(res, { "warm: 200": (r) => r.status === 200 });
}

export function unique() {
  const prompt = `Request ${__VU}-${__ITER}-${Date.now()}: give one practical tip about ${topics[__ITER % topics.length]} for a team of ${2 + (__ITER % 9)} engineers.`;
  const res = chat(KEY_PLATFORM, prompt, true, { kind: "unique" });
  if (res.status === 200) {
    ttftCold.add(res.timings.waiting); // first byte of the SSE stream = first token
    const m = /"completion_tokens":(\d+)/.exec(res.body || "");
    if (m) tokens.add(Number(m[1]));
  }
  gatewayErrors.add(res.status >= 500);
  rateLimited.add(res.status === 429);
  check(res, { "unique: 200": (r) => r.status === 200, "unique: SSE done": (r) => r.status !== 200 || (r.body || "").includes("[DONE]") });
}

export function burst() {
  const res = chat(KEY_BURST, `Burst ${__VU}-${__ITER}: what is a runbook?`, false, { kind: "burst" });
  rateLimited.add(res.status === 429);
  gatewayErrors.add(res.status >= 500);
  check(res, { "burst: 200 or 429": (r) => r.status === 200 || r.status === 429 });
}
