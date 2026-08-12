package handler

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/itispx/whatsapp-proxy/config"
	"github.com/itispx/whatsapp-proxy/metrics"
)

type recordedPinCall struct {
	waID, appID string
	ttl         time.Duration
}

type fakePinStore struct {
	setCalls    []recordedPinCall
	deleteCalls []string
}

func (f *fakePinStore) SetPin(ctx context.Context, waID, appID string, ttl time.Duration) error {
	f.setCalls = append(f.setCalls, recordedPinCall{waID: waID, appID: appID, ttl: ttl})
	return nil
}

func (f *fakePinStore) DeletePin(ctx context.Context, waID string) error {
	f.deleteCalls = append(f.deleteCalls, waID)
	return nil
}

func newTestPin(store *fakePinStore) *Pin {
	return &Pin{
		cfg:      &config.Config{Proxy: config.Proxy{GlobalRate: 100}},
		registry: store,
		metrics:  metrics.New(prometheus.NewRegistry()),
		log:      discardLog(),
	}
}

// ServeHTTP checks wa_id before touching the rate limiter, so a nil limiter
// is safe here.
func TestPin_MissingWaID(t *testing.T) {
	h := newTestPin(&fakePinStore{})
	app := &config.App{ID: "app-a"}

	req := httptest.NewRequest("POST", "/v1/conversations//pin", nil)
	ctx := context.WithValue(req.Context(), appContextKey, app)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestPin_SetsDefaultTTL(t *testing.T) {
	store := &fakePinStore{}
	h := newTestPin(store)
	app := &config.App{ID: "app-a"}

	req := httptest.NewRequest("POST", "/v1/conversations/5511999999999/pin", bytes.NewReader(nil))
	req.SetPathValue("wa_id", "5511999999999")
	ctx := context.WithValue(req.Context(), appContextKey, app)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	h.handlePin(w, req, app.ID, "5511999999999")

	if w.Code != 204 {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if len(store.setCalls) != 1 {
		t.Fatalf("expected 1 SetPin call, got %d", len(store.setCalls))
	}
	call := store.setCalls[0]
	if call.appID != "app-a" || call.waID != "5511999999999" {
		t.Fatalf("unexpected call: %+v", call)
	}
	if call.ttl != defaultPinTTL {
		t.Fatalf("expected default TTL %v, got %v", defaultPinTTL, call.ttl)
	}
}

func TestPin_CustomTTLCappedAt1Hour(t *testing.T) {
	store := &fakePinStore{}
	h := newTestPin(store)
	app := &config.App{ID: "app-a"}

	req := httptest.NewRequest("POST", "/v1/conversations/5511999999999/pin", bytes.NewReader([]byte(`{"ttl":999999}`)))
	req.SetPathValue("wa_id", "5511999999999")
	ctx := context.WithValue(req.Context(), appContextKey, app)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	h.handlePin(w, req, app.ID, "5511999999999")

	if len(store.setCalls) != 1 || store.setCalls[0].ttl != maxPinTTL {
		t.Fatalf("expected TTL capped at %v, got %+v", maxPinTTL, store.setCalls)
	}
}

func TestPin_CustomTTLWithinCap(t *testing.T) {
	store := &fakePinStore{}
	h := newTestPin(store)
	app := &config.App{ID: "app-a"}

	req := httptest.NewRequest("POST", "/v1/conversations/5511999999999/pin", bytes.NewReader([]byte(`{"ttl":1800}`)))
	req.SetPathValue("wa_id", "5511999999999")
	ctx := context.WithValue(req.Context(), appContextKey, app)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	h.handlePin(w, req, app.ID, "5511999999999")

	if len(store.setCalls) != 1 || store.setCalls[0].ttl != 30*time.Minute {
		t.Fatalf("expected 30m TTL, got %+v", store.setCalls)
	}
}

func TestPin_Unpin(t *testing.T) {
	store := &fakePinStore{}
	h := newTestPin(store)

	req := httptest.NewRequest("DELETE", "/v1/conversations/5511999999999/pin", nil)
	req.SetPathValue("wa_id", "5511999999999")
	w := httptest.NewRecorder()

	h.handleUnpin(w, req, "5511999999999")

	if w.Code != 204 {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if len(store.deleteCalls) != 1 || store.deleteCalls[0] != "5511999999999" {
		t.Fatalf("unexpected delete calls: %+v", store.deleteCalls)
	}
}
