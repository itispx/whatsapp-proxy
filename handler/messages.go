package handler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/itispx/whatsapp-proxy/config"
	"github.com/itispx/whatsapp-proxy/meta"
	"github.com/itispx/whatsapp-proxy/metrics"
	"github.com/itispx/whatsapp-proxy/ratelimit"
	"github.com/itispx/whatsapp-proxy/router"
	"github.com/itispx/whatsapp-proxy/snippet"
)

// registryWriter is the subset of registry.Registry used to record reply
// candidate evidence. Declared as an interface so the handler can be unit
// tested without Redis.
type registryWriter interface {
	WriteContext(ctx context.Context, messageID, appID, to string, expectsReply bool, topic, snippetText string, sentAt time.Time, replyTTL time.Duration) error
}

type Messages struct {
	cfg        *config.Config
	metaClient *meta.Client
	limiter    *ratelimit.Limiter
	router     *router.Router
	registry   registryWriter
	metrics    *metrics.Metrics
	log        *slog.Logger
}

func NewMessages(
	cfg *config.Config,
	metaClient *meta.Client,
	limiter *ratelimit.Limiter,
	rtr *router.Router,
	reg registryWriter,
	m *metrics.Metrics,
	log *slog.Logger,
) *Messages {
	return &Messages{
		cfg:        cfg,
		metaClient: metaClient,
		limiter:    limiter,
		router:     rtr,
		registry:   reg,
		metrics:    m,
		log:        log,
	}
}

func (h *Messages) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	app := appFromContext(r)
	ctx := r.Context()

	h.log.Debug("messages: handling send request", "app_id", app.ID, "app_name", app.Name)

	// Per-app rate limit, fallback to global.
	limit := h.cfg.Proxy.GlobalRate
	if app.Rate > 0 {
		limit = app.Rate
		h.log.Debug("messages: using per-app rate limit", "app_id", app.ID, "limit", limit)
	} else {
		h.log.Debug("messages: using global rate limit", "app_id", app.ID, "limit", limit)
	}

	allowed, err := h.limiter.Allow(ctx, "app:"+app.ID, limit)
	if err != nil {
		h.log.Error("rate limiter error", "app", app.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	h.log.Debug("messages: per-app rate limit checked", "app_id", app.ID, "allowed", allowed)

	// Also check global bucket when app has its own rate.
	if allowed && app.Rate > 0 {
		h.log.Debug("messages: checking global rate limit", "app_id", app.ID)
		allowed, err = h.limiter.Allow(ctx, "global", h.cfg.Proxy.GlobalRate)
		if err != nil {
			h.log.Error("global rate limiter error", "app", app.ID, "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		h.log.Debug("messages: global rate limit checked", "app_id", app.ID, "allowed", allowed)
	}

	if !allowed {
		h.log.Debug("messages: rate limit exceeded, rejecting request", "app_id", app.ID)
		h.metrics.MessagesSent.WithLabelValues(app.ID, "rate_limited").Inc()
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}

	sendMeta, err := ParseSendHeaders(r.Header)
	if err != nil {
		h.log.Debug("messages: invalid X-Proxy-* header", "app_id", app.ID, "err", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	h.log.Debug("messages: reading request body", "app_id", app.ID)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	h.log.Debug("messages: forwarding to meta", "app_id", app.ID, "body_size", len(body))

	start := time.Now()
	result, err := h.metaClient.SendMessage(ctx, body)
	h.metrics.MessageSendDuration.WithLabelValues(app.ID).Observe(time.Since(start).Seconds())

	if err != nil {
		h.log.Error("meta send error", "app", app.ID, "err", err)
		h.metrics.MessagesSent.WithLabelValues(app.ID, "upstream_error").Inc()
		writeError(w, http.StatusBadGateway, "failed to send message")
		return
	}

	h.log.Debug("messages: meta responded", "app_id", app.ID, "message_count", len(result.Messages))

	extracted, hasRecipient := snippet.Extract(body)
	if !hasRecipient {
		h.log.Debug("messages: no recipient in body, skipping registry writes", "app_id", app.ID)
	}

	snippetText := extracted.Snippet
	if !app.SnippetsEnabled() {
		snippetText = ""
	}

	// Map each message_id → app_id for routing status callbacks, and record
	// reply-candidate evidence for inbound attribution.
	for _, msg := range result.Messages {
		h.log.Debug("messages: storing routing mapping", "app_id", app.ID, "message_id", msg.ID)
		if err := h.router.Store(ctx, msg.ID, app.ID); err != nil {
			h.log.Error("router store error", "app", app.ID, "message_id", msg.ID, "err", err)
		}

		if hasRecipient {
			err := h.registry.WriteContext(
				ctx, msg.ID, app.ID, extracted.To,
				sendMeta.ExpectsReply, sendMeta.Topic, snippetText,
				time.Now(), sendMeta.ReplyTTL,
			)
			if err != nil {
				h.log.Error("registry write error", "app", app.ID, "message_id", msg.ID, "err", err)
			}
		}
	}

	h.metrics.MessagesSent.WithLabelValues(app.ID, "success").Inc()
	h.log.Info("message sent", "app", app.ID, "messages", len(result.Messages))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}
