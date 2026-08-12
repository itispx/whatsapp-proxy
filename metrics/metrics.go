package metrics

import "github.com/prometheus/client_golang/prometheus"

type Metrics struct {
	// MessagesSent counts outbound /v1/messages requests.
	// status: "success" | "rate_limited" | "upstream_error"
	MessagesSent *prometheus.CounterVec

	// AuthFailures counts rejected auth attempts.
	// reason: "missing" | "invalid"
	AuthFailures *prometheus.CounterVec

	// WebhookDeliveries counts stream worker delivery outcomes.
	// status: "success" | "dead_letter"
	WebhookDeliveries *prometheus.CounterVec

	// WebhookEvents counts inbound Meta events processed by the webhook handler.
	// type: "status_update" | "inbound_message"
	WebhookEvents *prometheus.CounterVec

	// MessageSendDuration observes the latency of Meta API calls.
	MessageSendDuration *prometheus.HistogramVec

	// InboundAttribution counts inbound messages processed by the attribution
	// ladder, by resolution level.
	// level: "exact" | "pinned" | "inferred" | "ambiguous" | "unknown"
	InboundAttribution *prometheus.CounterVec
}

func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		MessagesSent: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "whatsapp_proxy_messages_sent_total",
			Help: "Outbound messages forwarded to Meta, by app and outcome.",
		}, []string{"app_id", "status"}),

		AuthFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "whatsapp_proxy_auth_failures_total",
			Help: "Authentication failures on POST /v1/messages.",
		}, []string{"reason"}),

		WebhookDeliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "whatsapp_proxy_webhook_deliveries_total",
			Help: "Webhook delivery attempts by the stream worker, by app and outcome.",
		}, []string{"app_id", "status"}),

		WebhookEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "whatsapp_proxy_webhook_events_total",
			Help: "Inbound Meta webhook events received and processed.",
		}, []string{"type"}),

		MessageSendDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "whatsapp_proxy_message_send_duration_seconds",
			Help:    "Latency of outbound Meta API calls.",
			Buckets: prometheus.DefBuckets,
		}, []string{"app_id"}),

		InboundAttribution: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "whatsapp_proxy_inbound_attribution_total",
			Help: "Inbound messages processed by the attribution ladder, by resolution level.",
		}, []string{"level"}),
	}

	reg.MustRegister(
		m.MessagesSent,
		m.AuthFailures,
		m.WebhookDeliveries,
		m.WebhookEvents,
		m.MessageSendDuration,
		m.InboundAttribution,
	)

	// Pre-initialize every level so they're visible at /metrics before the
	// first inbound message of each kind arrives.
	for _, level := range []string{"exact", "pinned", "inferred", "ambiguous", "unknown"} {
		m.InboundAttribution.WithLabelValues(level).Add(0)
	}

	return m
}
