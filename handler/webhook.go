package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/itispx/whatsapp-proxy/attribution"
	"github.com/itispx/whatsapp-proxy/config"
	"github.com/itispx/whatsapp-proxy/metrics"
)

// metaPayload is used to extract message IDs for routing and, when enrichment
// is enabled, the fields needed to attribute inbound messages. The raw body
// is forwarded as-is to app webhooks and embedded byte-identical in the
// enrichment envelope.
type metaPayload struct {
	Entry []struct {
		Changes []struct {
			Value metaValue `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

type metaValue struct {
	Contacts []metaContact `json:"contacts"`
	Statuses []struct {
		ID string `json:"id"`
	} `json:"statuses"`
	Messages []metaMessage `json:"messages"`
}

type metaContact struct {
	WaID string `json:"wa_id"`
}

type metaMessage struct {
	ID      string `json:"id"`
	From    string `json:"from"`
	Context struct {
		ID string `json:"id"`
	} `json:"context"`
}

// statusRouter is the subset of router.Router used to route status callbacks.
// Declared as an interface so the handler can be unit tested without Redis.
type statusRouter interface {
	Lookup(ctx context.Context, messageID string) (string, bool, error)
}

// enqueuer is the subset of stream.Producer used to hand off deliveries.
// Declared as an interface so the handler can be unit tested without Redis.
type enqueuer interface {
	Enqueue(ctx context.Context, appID string, payload []byte) error
}

type Webhook struct {
	cfg      *config.Config
	router   statusRouter
	producer enqueuer
	metrics  *metrics.Metrics
	resolver *attribution.Resolver
	log      *slog.Logger
}

func NewWebhook(
	cfg *config.Config,
	rtr statusRouter,
	producer enqueuer,
	m *metrics.Metrics,
	resolver *attribution.Resolver,
	log *slog.Logger,
) *Webhook {
	return &Webhook{cfg: cfg, router: rtr, producer: producer, metrics: m, resolver: resolver, log: log}
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
			if len(val.Messages) > 0 {
				payload := body
				if h.cfg.MessageReceiver.Enrichment {
					payload = h.buildEnrichedPayload(ctx, val, body)
				}
				h.log.Debug("webhook: enqueuing to message_receiver")
				if err := h.producer.Enqueue(ctx, "message_receiver", payload); err != nil {
					h.log.Error("enqueue message_receiver error", "err", err)
				} else {
					h.metrics.WebhookEvents.WithLabelValues("inbound_message").Inc()
					h.log.Debug("webhook: message_receiver enqueued")
				}
			} else if len(val.Statuses) == 0 {
				// Any other Meta event: keep today's raw format regardless of enrichment.
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

// buildEnrichedPayload runs the attribution ladder for the first inbound
// message in val and wraps the raw body in an enrichment envelope. On any
// failure it degrades to the raw body rather than dropping the message.
func (h *Webhook) buildEnrichedPayload(ctx context.Context, val metaValue, body []byte) []byte {
	msg := val.Messages[0]

	waID := msg.From
	if len(val.Contacts) > 0 && val.Contacts[0].WaID != "" {
		waID = val.Contacts[0].WaID
	}

	res := h.resolver.Resolve(ctx, waID, msg.Context.ID)
	h.metrics.InboundAttribution.WithLabelValues(res.Level).Inc()
	h.log.Info("inbound attributed", "level", res.Level, "app_id", res.ResolvedAppID, "wa_id", waID)

	env := attribution.BuildEnvelope(res, json.RawMessage(body))
	out, err := json.Marshal(env)
	if err != nil {
		h.log.Error("failed to marshal enrichment envelope", "err", err)
		return body
	}
	return out
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
