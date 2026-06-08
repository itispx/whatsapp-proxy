package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Limiter implements a sliding window rate limiter backed by Redis.
// It uses a sorted set per key where each member is a unique request timestamp.
type Limiter struct {
	rdb *redis.Client
}

func New(rdb *redis.Client) *Limiter {
	return &Limiter{rdb: rdb}
}

// Allow returns true if the key is within its limit of maxReqs per minute.
// It atomically removes old entries, counts current, and adds the new one.
func (l *Limiter) Allow(ctx context.Context, key string, maxReqs int) (bool, error) {
	now := time.Now().UnixNano()
	windowStart := now - int64(time.Minute)
	redisKey := fmt.Sprintf("rate:%s", key)

	pipe := l.rdb.Pipeline()
	pipe.ZRemRangeByScore(ctx, redisKey, "0", fmt.Sprintf("%d", windowStart))
	countCmd := pipe.ZCard(ctx, redisKey)
	pipe.ZAdd(ctx, redisKey, redis.Z{Score: float64(now), Member: now})
	pipe.Expire(ctx, redisKey, 2*time.Minute)

	if _, err := pipe.Exec(ctx); err != nil {
		return false, fmt.Errorf("rate limit pipeline: %w", err)
	}

	return countCmd.Val() < int64(maxReqs), nil
}
