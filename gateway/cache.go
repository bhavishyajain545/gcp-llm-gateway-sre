package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
)

// CachedResponse is what a cache hit returns; it is enough to synthesize a
// full OpenAI-style response or a single streamed chunk.
type CachedResponse struct {
	Text         string    `json:"text"`
	Model        string    `json:"model"`
	Usage        ChatUsage `json:"usage"`
	FinishReason string    `json:"finish_reason"`
	CreatedAt    int64     `json:"created_at"`
}

// Cache has two layers in Redis, both namespaced per tenant+model so one
// tenant can never be served another tenant's answer:
//
//	exact    ex:<ns>:<sha256(request)>              -> response JSON (TTL)
//	semantic sem:<ns>:idx (ZSET id by time)         bounded window of recent prompts
//	         sem:<ns>:v:<id> (unit vector, float32) -> cosine = dot product
//	         sem:<ns>:r:<id>                        -> response JSON
//
// The semantic layer is a brute-force scan over at most SemanticMax entries,
// which is exact and cheap at this size (2000 × 256 floats ≈ 2 MB per
// namespace). Past that, swap in Memorystore for Valkey vector search.
type Cache struct {
	rdb       *redis.Client
	ttl       time.Duration
	threshold float64
	max       int64
}

func NewCache(rdb *redis.Client, ttl time.Duration, threshold float64, max int64) *Cache {
	return &Cache{rdb: rdb, ttl: ttl, threshold: threshold, max: max}
}

func (c *Cache) GetExact(ctx context.Context, ns, key string) (*CachedResponse, error) {
	ctx, span := tracer.Start(ctx, "cache.exact")
	defer span.End()
	b, err := c.rdb.Get(ctx, "ex:"+ns+":"+key).Bytes()
	if errors.Is(err, redis.Nil) {
		span.SetAttributes(attribute.Bool("cache.hit", false))
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r CachedResponse
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	span.SetAttributes(attribute.Bool("cache.hit", true))
	return &r, nil
}

func (c *Cache) PutExact(ctx context.Context, ns, key string, r *CachedResponse) error {
	b, _ := json.Marshal(r)
	return c.rdb.Set(ctx, "ex:"+ns+":"+key, b, c.ttl).Err()
}

// GetSemantic returns the best cached answer whose prompt vector is at least
// `threshold` cosine-similar to vec, and the score.
func (c *Cache) GetSemantic(ctx context.Context, ns string, vec []float32) (*CachedResponse, float64, error) {
	ctx, span := tracer.Start(ctx, "cache.semantic")
	defer span.End()
	idx := "sem:" + ns + ":idx"
	ids, err := c.rdb.ZRevRange(ctx, idx, 0, c.max-1).Result()
	if err != nil || len(ids) == 0 {
		return nil, 0, err
	}
	pipe := c.rdb.Pipeline()
	cmds := make([]*redis.StringCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.Get(ctx, "sem:"+ns+":v:"+id)
	}
	_, _ = pipe.Exec(ctx) // individual misses are redis.Nil, handled below
	best, bestID := -1.0, ""
	var expired []any
	for i, cmd := range cmds {
		b, err := cmd.Bytes()
		if err != nil {
			expired = append(expired, ids[i])
			continue
		}
		if s := dot(vec, decodeVec(b)); s > best {
			best, bestID = s, ids[i]
		}
	}
	if len(expired) > 0 {
		_ = c.rdb.ZRem(ctx, idx, expired...).Err()
	}
	span.SetAttributes(attribute.Int("cache.candidates", len(ids)), attribute.Float64("cache.best_score", best))
	if bestID == "" || best < c.threshold {
		return nil, best, nil
	}
	b, err := c.rdb.Get(ctx, "sem:"+ns+":r:"+bestID).Bytes()
	if err != nil {
		return nil, best, nil
	}
	var r CachedResponse
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, best, err
	}
	span.SetAttributes(attribute.Bool("cache.hit", true))
	return &r, best, nil
}

func (c *Cache) PutSemantic(ctx context.Context, ns, id string, vec []float32, r *CachedResponse) error {
	b, _ := json.Marshal(r)
	pipe := c.rdb.Pipeline()
	pipe.Set(ctx, "sem:"+ns+":v:"+id, encodeVec(vec), c.ttl)
	pipe.Set(ctx, "sem:"+ns+":r:"+id, b, c.ttl)
	pipe.ZAdd(ctx, "sem:"+ns+":idx", redis.Z{Score: float64(time.Now().UnixNano()), Member: id})
	pipe.ZRemRangeByRank(ctx, "sem:"+ns+":idx", 0, -(c.max + 1)) // keep the newest `max`
	pipe.Expire(ctx, "sem:"+ns+":idx", c.ttl)
	_, err := pipe.Exec(ctx)
	return err
}

func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

func decodeVec(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}
