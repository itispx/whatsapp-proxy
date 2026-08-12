package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/itispx/whatsapp-proxy/attribution"
	"github.com/itispx/whatsapp-proxy/config"
	"github.com/itispx/whatsapp-proxy/metrics"
)

const testAppSecret = "test-app-secret"

type fakeStatusRouter struct {
	mapping map[string]string
}

func (f *fakeStatusRouter) Lookup(ctx context.Context, messageID string) (string, bool, error) {
	appID, found := f.mapping[messageID]
	return appID, found, nil
}

type recordedEnqueue struct {
	appID   string
	payload []byte
}

type fakeEnqueuer struct {
	calls []recordedEnqueue
}

func (f *fakeEnqueuer) Enqueue(ctx context.Context, appID string, payload []byte) error {
	f.calls = append(f.calls, recordedEnqueue{appID: appID, payload: append([]byte(nil), payload...)})
	return nil
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fakeCandidateStore struct {
	candidates map[string][]attribution.Candidate
}

func (f *fakeCandidateStore) Candidates(ctx context.Context, waID string) ([]attribution.Candidate, error) {
	return f.candidates[waID], nil
}

func (f *fakeCandidateStore) GetPin(ctx context.Context, waID string) (string, bool, error) {
	return "", false, nil
}

func (f *fakeCandidateStore) SetPin(ctx context.Context, waID, appID string, ttl time.Duration) error {
	return nil
}

func (f *fakeCandidateStore) RefreshPin(ctx context.Context, waID string, ttl time.Duration) error {
	return nil
}

func newTestWebhook(t *testing.T, enrichment bool, router *fakeStatusRouter) (*Webhook, *fakeEnqueuer) {
	t.Helper()

	cfg := &config.Config{
		Meta:            config.Meta{AppSecret: testAppSecret},
		MessageReceiver: config.MessageReceiver{WebhookURL: "http://message-receiver", Enrichment: enrichment},
	}

	m := metrics.New(prometheus.NewRegistry())
	log := discardLog()
	enq := &fakeEnqueuer{}
	store := &fakeCandidateStore{}
	resolver := attribution.NewResolver(router, store, log)

	return NewWebhook(cfg, router, enq, m, resolver, log), enq
}

func newReader(body []byte) *bytes.Reader {
	return bytes.NewReader(body)
}

func computeHMAC(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// TestEnrichmentOff_ByteIdenticalPassthrough guards the non-negotiable:
// enrichment: false must produce today's behavior exactly — the raw Meta
// payload enqueued to message_receiver unchanged.
func TestEnrichmentOff_ByteIdenticalPassthrough(t *testing.T) {
	body := []byte(`{"object":"whatsapp_business_account","entry":[{"changes":[{"value":{"messages":[{"id":"wamid.1","from":"5511999999999","type":"text","text":{"body":"hi"}}]}}]}]}`)

	router := &fakeStatusRouter{mapping: map[string]string{}}
	h, enq := newTestWebhook(t, false, router)

	req := httptest.NewRequest("POST", "/webhook", newReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+computeHMAC(body, testAppSecret))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if len(enq.calls) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(enq.calls))
	}
	call := enq.calls[0]
	if call.appID != "message_receiver" {
		t.Fatalf("expected appID message_receiver, got %q", call.appID)
	}
	if string(call.payload) != string(body) {
		t.Fatalf("payload not byte-identical:\nwant %s\ngot  %s", body, call.payload)
	}
}

// TestEnrichmentOn_ExactAttribution verifies rung 2 of the ladder and that the
// raw payload is still embedded byte-identical inside the envelope.
func TestEnrichmentOn_ExactAttribution(t *testing.T) {
	body := []byte(`{"object":"whatsapp_business_account","entry":[{"changes":[{"value":{"messages":[{"id":"wamid.2","from":"5511999999999","type":"text","text":{"body":"hi"},"context":{"id":"wamid.original"}}]}}]}]}`)

	router := &fakeStatusRouter{mapping: map[string]string{"wamid.original": "app-a"}}
	h, enq := newTestWebhook(t, true, router)

	req := httptest.NewRequest("POST", "/webhook", newReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+computeHMAC(body, testAppSecret))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if len(enq.calls) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(enq.calls))
	}

	var env attribution.Envelope
	if err := json.Unmarshal(enq.calls[0].payload, &env); err != nil {
		t.Fatalf("failed to unmarshal envelope: %v", err)
	}
	if env.Attribution != attribution.LevelExact {
		t.Fatalf("expected attribution %q, got %q", attribution.LevelExact, env.Attribution)
	}
	if env.ResolvedAppID == nil || *env.ResolvedAppID != "app-a" {
		t.Fatalf("expected resolved_app_id app-a, got %v", env.ResolvedAppID)
	}
	if string(env.Payload) != string(body) {
		t.Fatalf("embedded payload not byte-identical:\nwant %s\ngot  %s", body, env.Payload)
	}
}

// TestEnrichmentOn_UnknownWhenNoEvidence verifies rung 5 when there is no
// context.id and no candidate history for the sender.
func TestEnrichmentOn_UnknownWhenNoEvidence(t *testing.T) {
	body := []byte(`{"object":"whatsapp_business_account","entry":[{"changes":[{"value":{"messages":[{"id":"wamid.3","from":"5511999999999","type":"text","text":{"body":"hi"}}]}}]}]}`)

	router := &fakeStatusRouter{mapping: map[string]string{}}
	h, enq := newTestWebhook(t, true, router)

	req := httptest.NewRequest("POST", "/webhook", newReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+computeHMAC(body, testAppSecret))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	var env attribution.Envelope
	if err := json.Unmarshal(enq.calls[0].payload, &env); err != nil {
		t.Fatalf("failed to unmarshal envelope: %v", err)
	}
	if env.Attribution != attribution.LevelUnknown {
		t.Fatalf("expected attribution %q, got %q", attribution.LevelUnknown, env.Attribution)
	}
	if env.ResolvedAppID != nil {
		t.Fatalf("expected nil resolved_app_id, got %q", *env.ResolvedAppID)
	}
	if len(env.Candidates) != 0 {
		t.Fatalf("expected no candidates, got %d", len(env.Candidates))
	}
}
