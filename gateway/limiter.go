package main

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// Limiter implements per-tenant request rate limits (token bucket, atomic in
// Redis via Lua so every replica shares one bucket) and per-tenant daily
// token budgets. Redis errors fail OPEN: a cache outage must degrade cost
// controls, not availability. The failures are counted (llm_limiter_errors_total).
type Limiter struct {
	rdb    *redis.Client
	bucket *redis.Script
}

// KEYS[1] bucket key; ARGV: refill per second, burst, now (float seconds)
// returns {allowed(0/1), remaining, retry_after_seconds}
const bucketLua = `
local key = KEYS[1]
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local b = redis.call('HMGET', key, 'tokens', 'ts')
local tokens = tonumber(b[1])
local ts = tonumber(b[2])
if tokens == nil then tokens = burst; ts = now end
local delta = math.max(0, now - ts)
tokens = math.min(burst, tokens + delta * rate)
local allowed = 0
if tokens >= 1 then tokens = tokens - 1; allowed = 1 end
redis.call('HSET', key, 'tokens', tokens, 'ts', now)
redis.call('EXPIRE', key, math.ceil(burst / rate) + 60)
local retry = 0
if allowed == 0 then retry = math.ceil((1 - tokens) / rate) end
return {allowed, math.floor(tokens), retry}
`

func NewLimiter(rdb *redis.Client) *Limiter {
	return &Limiter{rdb: rdb, bucket: redis.NewScript(bucketLua)}
}

// Allow consumes one request from the tenant's bucket.
func (l *Limiter) Allow(ctx context.Context, tenant string, rpm, burst int) (allowed bool, remaining, retryAfter int, err error) {
	ctx, span := tracer.Start(ctx, "limiter.allow")
	defer span.End()
	rate := float64(rpm) / 60.0
	now := float64(time.Now().UnixNano()) / 1e9
	res, err := l.bucket.Run(ctx, l.rdb, []string{"rl:" + tenant}, rate, burst, now).Int64Slice()
	if err != nil {
		limiterErrors.WithLabelValues("bucket").Inc()
		return true, burst, 0, err // fail open
	}
	return res[0] == 1, int(res[1]), int(res[2]), nil
}

func budgetKey(tenant string, t time.Time) string {
	return "tok:" + tenant + ":" + t.UTC().Format("20060102")
}

// BudgetUsed returns tokens consumed by the tenant today (UTC).
func (l *Limiter) BudgetUsed(ctx context.Context, tenant string) (int64, error) {
	n, err := l.rdb.Get(ctx, budgetKey(tenant, time.Now())).Int64()
	if err == redis.Nil {
		return 0, nil
	}
	if err != nil {
		limiterErrors.WithLabelValues("budget_get").Inc()
		return 0, err
	}
	return n, nil
}

// BudgetAdd records tokens after a completed request (the exact count is only
// known afterwards, so a tenant can overshoot by at most one request).
func (l *Limiter) BudgetAdd(ctx context.Context, tenant string, n int64) {
	if n <= 0 {
		return
	}
	k := budgetKey(tenant, time.Now())
	pipe := l.rdb.Pipeline()
	pipe.IncrBy(ctx, k, n)
	pipe.Expire(ctx, k, 48*time.Hour)
	if _, err := pipe.Exec(ctx); err != nil {
		limiterErrors.WithLabelValues("budget_add").Inc()
	}
}
