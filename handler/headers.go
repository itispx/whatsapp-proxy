package handler

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultReplyTTL = 24 * time.Hour
	maxReplyTTL     = 48 * time.Hour
)

// SendMeta is the send-time metadata an app may declare on POST /v1/messages
// via X-Proxy-* headers. It never affects what is forwarded to Meta.
type SendMeta struct {
	ExpectsReply bool
	Topic        string
	ReplyTTL     time.Duration
}

// ParseSendHeaders reads and validates the optional X-Proxy-* headers.
// All headers are optional; an app that sends none gets SendMeta's zero
// behavior: ExpectsReply false, no topic, the default 24h reply TTL.
func ParseSendHeaders(h http.Header) (SendMeta, error) {
	meta := SendMeta{ReplyTTL: defaultReplyTTL}

	if raw := strings.TrimSpace(h.Get("X-Proxy-Expects-Reply")); raw != "" {
		switch strings.ToLower(raw) {
		case "true", "1":
			meta.ExpectsReply = true
		case "false", "0":
			meta.ExpectsReply = false
		default:
			return SendMeta{}, fmt.Errorf("invalid X-Proxy-Expects-Reply value %q", raw)
		}
	}

	meta.Topic = h.Get("X-Proxy-Topic")

	if raw := strings.TrimSpace(h.Get("X-Proxy-Reply-TTL")); raw != "" {
		secs, err := strconv.Atoi(raw)
		if err != nil || secs <= 0 {
			return SendMeta{}, fmt.Errorf("invalid X-Proxy-Reply-TTL value %q", raw)
		}
		ttl := time.Duration(secs) * time.Second
		if ttl > maxReplyTTL {
			ttl = maxReplyTTL
		}
		meta.ReplyTTL = ttl
	}

	return meta, nil
}
