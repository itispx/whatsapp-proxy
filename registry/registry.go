// Package registry stores per-message and per-conversation context in Redis
// so the attribution ladder has evidence to work from: msgctx: (what an
// outbound message was about), convo: (recent outbound history per user),
// and pin: (session pinning, see the attribution package).
package registry

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/itispx/whatsapp-proxy/attribution"
)

const (
	convoTTL     = 48 * time.Hour
	pinOnSendTTL = 15 * time.Minute
)

// Registry reads and writes the msgctx:/convo:/pin: Redis keys.
type Registry struct {
	rdb *redis.Client
	log *slog.Logger
}

func New(rdb *redis.Client, log *slog.Logger) *Registry {
	return &Registry{rdb: rdb, log: log}
}

// WriteContext records an outbound message as reply-candidate evidence:
// the msgctx: hash (with its own reply_ttl), an entry in the sender's
// convo: sorted set (pruned lazily to the last 48h), and pins the
// conversation to appID — an outbound send pins the conversation to its
// sender; a later send from a different app overwrites the pin.
func (r *Registry) WriteContext(
	ctx context.Context,
	messageID, appID, to string,
	expectsReply bool,
	topic, snippet string,
	sentAt time.Time,
	replyTTL time.Duration,
) error {
	expectsReplyVal := "0"
	if expectsReply {
		expectsReplyVal = "1"
	}

	convoK := convoKey(to)

	pipe := r.rdb.Pipeline()
	pipe.HSet(ctx, msgctxKey(messageID), map[string]any{
		"app_id":        appID,
		"to":            to,
		"expects_reply": expectsReplyVal,
		"topic":         topic,
		"snippet":       snippet,
		"sent_at":       sentAt.Unix(),
	})
	pipe.Expire(ctx, msgctxKey(messageID), replyTTL)
	pipe.ZAdd(ctx, convoK, redis.Z{Score: float64(sentAt.Unix()), Member: messageID})
	pipe.Expire(ctx, convoK, convoTTL)
	pipe.ZRemRangeByScore(ctx, convoK, "0", fmt.Sprintf("%d", sentAt.Add(-convoTTL).Unix()))
	pipe.Set(ctx, pinKey(to), appID, pinOnSendTTL)

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("registry write context: %w", err)
	}
	return nil
}

// GetPin returns the app_id currently pinned to waID's conversation.
func (r *Registry) GetPin(ctx context.Context, waID string) (string, bool, error) {
	val, err := r.rdb.Get(ctx, pinKey(waID)).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("registry get pin: %w", err)
	}
	return val, true, nil
}

// SetPin pins waID's conversation to appID for ttl.
func (r *Registry) SetPin(ctx context.Context, waID, appID string, ttl time.Duration) error {
	if err := r.rdb.Set(ctx, pinKey(waID), appID, ttl).Err(); err != nil {
		return fmt.Errorf("registry set pin: %w", err)
	}
	return nil
}

// RefreshPin slides an existing pin's TTL forward without changing its value.
func (r *Registry) RefreshPin(ctx context.Context, waID string, ttl time.Duration) error {
	if err := r.rdb.Expire(ctx, pinKey(waID), ttl).Err(); err != nil {
		return fmt.Errorf("registry refresh pin: %w", err)
	}
	return nil
}

// DeletePin removes any pin on waID's conversation.
func (r *Registry) DeletePin(ctx context.Context, waID string) error {
	if err := r.rdb.Del(ctx, pinKey(waID)).Err(); err != nil {
		return fmt.Errorf("registry delete pin: %w", err)
	}
	return nil
}

// Candidates assembles up to the 10 most recent outbound messages sent to
// waID, sorted flagged-first then newest-first. msgctx: entries that have
// already expired (shorter TTL than convo:) are silently skipped.
func (r *Registry) Candidates(ctx context.Context, waID string) ([]attribution.Candidate, error) {
	ids, err := r.rdb.ZRangeArgs(ctx, redis.ZRangeArgs{
		Key:   convoKey(waID),
		Start: 0,
		Stop:  9,
		Rev:   true,
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("registry candidates: zrange: %w", err)
	}
	if len(ids) == 0 {
		return nil, nil
	}

	pipe := r.rdb.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.HGetAll(ctx, msgctxKey(id))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, fmt.Errorf("registry candidates: hgetall: %w", err)
	}

	var out []attribution.Candidate
	for i, cmd := range cmds {
		m, err := cmd.Result()
		if err != nil && err != redis.Nil {
			r.log.Error("registry: hgetall error", "message_id", ids[i], "err", err)
			continue
		}
		if c, ok := candidateFromHash(ids[i], m); ok {
			out = append(out, c)
		}
	}

	sortCandidates(out)
	return out, nil
}

// candidateFromHash converts a msgctx: hash into a Candidate. ok is false
// for an empty hash — the id's msgctx: entry has already expired.
func candidateFromHash(messageID string, m map[string]string) (attribution.Candidate, bool) {
	if len(m) == 0 {
		return attribution.Candidate{}, false
	}
	sentAt, _ := strconv.ParseInt(m["sent_at"], 10, 64)
	return attribution.Candidate{
		AppID:        m["app_id"],
		MessageID:    messageID,
		ExpectsReply: m["expects_reply"] == "1",
		Topic:        m["topic"],
		Snippet:      m["snippet"],
		SentAt:       sentAt,
	}, true
}

// sortCandidates orders flagged (expects_reply) candidates first, newest
// first within each group. Stable so ties keep their zset order.
func sortCandidates(c []attribution.Candidate) {
	sort.SliceStable(c, func(i, j int) bool {
		if c[i].ExpectsReply != c[j].ExpectsReply {
			return c[i].ExpectsReply
		}
		return c[i].SentAt > c[j].SentAt
	})
}

func msgctxKey(messageID string) string { return "msgctx:" + messageID }
func convoKey(waID string) string       { return "convo:" + waID }
func pinKey(waID string) string         { return "pin:" + waID }
