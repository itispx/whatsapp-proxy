package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/itispx/whatsapp-proxy/config"
	"github.com/itispx/whatsapp-proxy/metrics"
	"github.com/itispx/whatsapp-proxy/ratelimit"
)

const (
	defaultPinTTL = 15 * time.Minute
	maxPinTTL     = time.Hour
)

// pinStore is the subset of registry.Registry used to pin/unpin a
// conversation. Declared as an interface so the handler can be unit tested
// without Redis.
type pinStore interface {
	SetPin(ctx context.Context, waID, appID string, ttl time.Duration) error
	DeletePin(ctx context.Context, waID string) error
}

type pinRequest struct {
	TTL int `json:"ttl"`
}

// Pin handles POST/DELETE /v1/conversations/{wa_id}/pin. The pinned app is
// whichever app's API key authenticated the request.
type Pin struct {
	cfg      *config.Config
	limiter  *ratelimit.Limiter
	registry pinStore
	metrics  *metrics.Metrics
	log      *slog.Logger
}

func NewPin(
	cfg *config.Config,
	limiter *ratelimit.Limiter,
	reg pinStore,
	m *metrics.Metrics,
	log *slog.Logger,
) *Pin {
	return &Pin{cfg: cfg, limiter: limiter, registry: reg, metrics: m, log: log}
}

func (h *Pin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	app := appFromContext(r)
	ctx := r.Context()
	waID := r.PathValue("wa_id")

	if waID == "" {
		writeError(w, http.StatusBadRequest, "wa_id is required")
		return
	}

	// Same rate-limit bucket as POST /v1/messages.
	limit := h.cfg.Proxy.GlobalRate
	if app.Rate > 0 {
		limit = app.Rate
	}
	allowed, err := h.limiter.Allow(ctx, "app:"+app.ID, limit)
	if err == nil && allowed && app.Rate > 0 {
		allowed, err = h.limiter.Allow(ctx, "global", h.cfg.Proxy.GlobalRate)
	}
	if err != nil {
		h.log.Error("pin: rate limiter error", "app_id", app.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !allowed {
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}

	switch r.Method {
	case http.MethodPost:
		h.handlePin(w, r, app.ID, waID)
	case http.MethodDelete:
		h.handleUnpin(w, r, waID)
	default:
		http.NotFound(w, r)
	}
}

func (h *Pin) handlePin(w http.ResponseWriter, r *http.Request, appID, waID string) {
	ttl := defaultPinTTL

	if r.ContentLength != 0 {
		var body pinRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if body.TTL > 0 {
			ttl = time.Duration(body.TTL) * time.Second
			if ttl > maxPinTTL {
				ttl = maxPinTTL
			}
		}
	}

	if err := h.registry.SetPin(r.Context(), waID, appID, ttl); err != nil {
		h.log.Error("pin: set pin error", "app_id", appID, "wa_id", waID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	h.log.Info("conversation pinned", "app_id", appID, "wa_id", waID, "ttl", ttl)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Pin) handleUnpin(w http.ResponseWriter, r *http.Request, waID string) {
	if err := h.registry.DeletePin(r.Context(), waID); err != nil {
		h.log.Error("pin: delete pin error", "wa_id", waID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	h.log.Info("conversation unpinned", "wa_id", waID)
	w.WriteHeader(http.StatusNoContent)
}
