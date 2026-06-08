package router

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

const msgTTL = 24 * time.Hour

// Router maps message IDs to app IDs in Redis.
type Router struct {
	rdb *redis.Client
	log *slog.Logger
}

func New(rdb *redis.Client, log *slog.Logger) *Router {
	return &Router{rdb: rdb, log: log}
}

// Store saves the message_id → app_id mapping with a 24h TTL.
func (r *Router) Store(ctx context.Context, messageID, appID string) error {
	r.log.Debug("router: storing message mapping", "message_id", messageID, "app_id", appID)
	key := fmt.Sprintf("msg:%s", messageID)
	return r.rdb.Set(ctx, key, appID, msgTTL).Err()
}

// Lookup returns the app_id for a given message_id. Returns "", false if not found.
func (r *Router) Lookup(ctx context.Context, messageID string) (string, bool, error) {
	r.log.Debug("router: looking up message mapping", "message_id", messageID)
	key := fmt.Sprintf("msg:%s", messageID)
	val, err := r.rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		r.log.Debug("router: no mapping found", "message_id", messageID)
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("router lookup: %w", err)
	}
	r.log.Debug("router: mapping found", "message_id", messageID, "app_id", val)
	return val, true, nil
}
