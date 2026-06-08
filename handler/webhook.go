package handler

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/itispx/whatsapp-proxy/config"
	"github.com/itispx/whatsapp-proxy/metrics"
	"github.com/itispx/whatsapp-proxy/router"
	"github.com/itispx/whatsapp-proxy/stream"
)

// metaPayload is used only to extract message IDs for routing.
// The raw body is forwarded as-is to app webhooks.
type metaPayload struct {
	Entry []struct {
		Changes []struct {
			Value struct {
				Statuses []struct {
					ID string `json:"id"`
				} `json:"statuses"`
				Messages []struct {
					ID string `json:"id"`
				} `json:"messages"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

type Webhook struct {
	cfg      *config.Config
	router   *router.Router
	producer *stream.Producer
	metrics  *metrics.Metrics
	log      *slog.Logger
}

func NewWebhook(
	cfg *config.Config,
	rtr *router.Router,
	producer *stream.Producer,
	m *metrics.Metrics,
	log *slog.Logger,
) *Webhook {
	return &Webhook{cfg: cfg, router: rtr, producer: producer, metrics: m, log: log}
}

// ServeHTTP handles both GET (hub.challenge) and POST (events) from Meta.
func (h *Webhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.log.Debug("webhook: request received", "method", r.Method, "path", r.URL.Path)

	switch r.Method {
	case http.MethodGet:
		h.handleVerification(w, r)
	case http.MethodPost:
		h.handleEvent(w, r)
	default:
		h.log.Debug("webhook: unsupported method", "method", r.Method)
		http.NotFound(w, r)
	}
}

func (h *Webhook) handleVerification(w http.ResponseWriter, r *http.Request) {
	h.log.Debug("webhook: handling hub challenge verification")

	q := r.URL.Query()

	if q.Get("hub.mode") != "subscribe" {
		h.log.Debug("webhook: invalid hub.mode", "hub.mode", q.Get("hub.mode"))
		writeError(w, http.StatusBadRequest, "invalid hub.mode")
		return
	}

	if q.Get("hub.verify_token") != h.cfg.Meta.VerifyToken {
		h.log.Debug("webhook: verify_token mismatch")
		writeError(w, http.StatusForbidden, "invalid verify_token")
		return
	}

	h.log.Debug("webhook: hub challenge verified, responding with challenge")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(q.Get("hub.challenge")))
}

func (h *Webhook) handleEvent(w http.ResponseWriter, r *http.Request) {
	h.log.Debug("webhook: reading event body")

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read body")
		return
	}

	h.log.Debug("webhook: verifying signature", "body_size", len(body))

	if !h.verifySignature(r, body) {
		h.log.Warn("invalid webhook signature")
		writeError(w, http.StatusForbidden, "invalid signature")
		return
	}

	h.log.Debug("webhook: signature verified, responding 200 to meta")

	// Respond to Meta immediately — delivery to apps is async.
	w.WriteHeader(http.StatusOK)

	var payload metaPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		h.log.Error("failed to parse meta payload", "err", err)
		return
	}

	h.log.Debug("webhook: payload parsed", "entry_count", len(payload.Entry))

	ctx := r.Context()

	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			val := change.Value

			h.log.Debug("webhook: processing change", "status_count", len(val.Statuses), "message_count", len(val.Messages))

			// Status updates: route to the app that sent the original message.
			for _, status := range val.Statuses {
				h.log.Debug("webhook: looking up app for status update", "message_id", status.ID)

				appID, found, err := h.router.Lookup(ctx, status.ID)
				if err != nil {
					h.log.Error("router lookup error", "message_id", status.ID, "err", err)
					continue
				}
				if !found {
					h.log.Warn("no app mapping for message_id", "message_id", status.ID)
					continue
				}

				h.log.Debug("webhook: app found for status update, enqueuing", "message_id", status.ID, "app_id", appID)

				app, ok := h.cfg.AppByID(appID)
				if !ok {
					h.log.Warn("app not found in config", "app_id", appID)
					continue
				}
				if err := h.producer.Enqueue(ctx, app.ID, body); err != nil {
					h.log.Error("enqueue error", "app_id", appID, "err", err)
				} else {
					h.metrics.WebhookEvents.WithLabelValues("status_update").Inc()
					h.log.Debug("webhook: status update enqueued", "app_id", appID)
				}
			}

			// Inbound messages and any other events → message_receiver webhook.
			if len(val.Messages) > 0 || (len(val.Statuses) == 0 && len(val.Messages) == 0) {
				h.log.Debug("webhook: enqueuing to message_receiver")
				if err := h.producer.Enqueue(ctx, "message_receiver", body); err != nil {
					h.log.Error("enqueue message_receiver error", "err", err)
				} else {
					h.metrics.WebhookEvents.WithLabelValues("inbound_message").Inc()
					h.log.Debug("webhook: message_receiver enqueued")
				}
			}
		}
	}
}

func (h *Webhook) verifySignature(r *http.Request, body []byte) bool {
	sig := r.Header.Get("X-Hub-Signature-256")
	expected, ok := strings.CutPrefix(sig, "sha256=")
	if !ok {
		h.log.Debug("webhook: X-Hub-Signature-256 header missing or malformed")
		return false
	}

	mac := hmac.New(sha256.New, []byte(h.cfg.Meta.AppSecret))
	mac.Write(body)
	actual := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expected), []byte(actual))
}
