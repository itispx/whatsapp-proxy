package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"
)

const (
	appID1 = "integration-app-1"
	appID2 = "integration-app-2"
)

// envelope mirrors attribution.Envelope for test assertions without importing
// the internal package from the black-box integration test module.
type envelope struct {
	Version       int             `json:"version"`
	Attribution   string          `json:"attribution"`
	ResolvedAppID *string         `json:"resolved_app_id"`
	Candidates    []candidate     `json:"candidates"`
	Payload       json.RawMessage `json:"payload"`
}

type candidate struct {
	AppID        string `json:"app_id"`
	MessageID    string `json:"message_id"`
	ExpectsReply bool   `json:"expects_reply"`
	Topic        string `json:"topic"`
	Snippet      string `json:"snippet"`
	SentAt       int64  `json:"sent_at"`
}

func TestAttribution_ExactViaContextID(t *testing.T) {
	loadEnv()

	appSecret := os.Getenv("META_APP_SECRET")
	if appSecret == "" {
		t.Skip("META_APP_SECRET not set")
	}
	to := os.Getenv("TEST_RECIPIENT")
	if to == "" {
		t.Fatal("TEST_RECIPIENT env var is required")
	}

	resetCallbacks(t, messageReceiverURL)

	resp := sendMessage(t, app1Key, to)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	msgID := parseMessageID(t, resp)
	t.Logf("outbound message sent: %s", msgID)

	payload := []byte(fmt.Sprintf(
		`{"object":"whatsapp_business_account","entry":[{"changes":[{"value":{"contacts":[{"wa_id":"%s"}],"messages":[{"id":"test-exact-%s","from":"%s","type":"text","text":{"body":"yes"},"context":{"id":"%s"}}]}}]}]}`,
		to, msgID, to, msgID,
	))

	postWebhook(t, appSecret, payload)

	env := waitForEnvelope(t, messageReceiverURL, func(e envelope) bool {
		return e.Attribution != "" && string(e.Payload) != ""
	}, 10*time.Second)

	if env.Attribution != "exact" {
		t.Fatalf("expected attribution 'exact', got %q", env.Attribution)
	}
	if env.ResolvedAppID == nil || *env.ResolvedAppID != appID1 {
		t.Fatalf("expected resolved_app_id %q, got %v", appID1, env.ResolvedAppID)
	}
}

// sendMessageWithHeaders sends an outbound message with optional X-Proxy-*
// attribution headers set.
func sendMessageWithHeaders(t *testing.T, apiKey, to string, headers map[string]string) *http.Response {
	t.Helper()
	body := fmt.Sprintf(
		`{"messaging_product":"whatsapp","to":"%s","type":"text","text":{"body":"integration test"}}`,
		to,
	)
	req, _ := http.NewRequest(http.MethodPost, proxyURL()+"/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send message: %v", err)
	}
	return resp
}

func TestAttribution_Inferred_OneFlaggedAppWins(t *testing.T) {
	loadEnv()

	appSecret := os.Getenv("META_APP_SECRET")
	if appSecret == "" {
		t.Skip("META_APP_SECRET not set")
	}
	to := os.Getenv("TEST_RECIPIENT")
	if to == "" {
		t.Fatal("TEST_RECIPIENT env var is required")
	}

	resetCallbacks(t, messageReceiverURL)

	// App A flags expects_reply, app B doesn't.
	respA := sendMessageWithHeaders(t, app1Key, to, map[string]string{
		"X-Proxy-Expects-Reply": "true",
		"X-Proxy-Topic":         "order-1234-shipping",
	})
	defer respA.Body.Close()
	if respA.StatusCode != http.StatusOK {
		t.Fatalf("app1 send: expected 200, got %d", respA.StatusCode)
	}

	respB := sendMessage(t, app2Key, to)
	defer respB.Body.Close()
	if respB.StatusCode != http.StatusOK {
		t.Fatalf("app2 send: expected 200, got %d", respB.StatusCode)
	}

	payload := []byte(fmt.Sprintf(
		`{"object":"whatsapp_business_account","entry":[{"changes":[{"value":{"contacts":[{"wa_id":"%s"}],"messages":[{"id":"test-inferred-%s","from":"%s","type":"text","text":{"body":"yes friday works"}}]}}]}]}`,
		to, to, to,
	))
	postWebhook(t, appSecret, payload)

	env := waitForEnvelope(t, messageReceiverURL, func(e envelope) bool {
		return e.Attribution == "inferred" || e.Attribution == "ambiguous"
	}, 10*time.Second)

	if env.Attribution != "inferred" {
		t.Fatalf("expected attribution 'inferred', got %q", env.Attribution)
	}
	if env.ResolvedAppID == nil || *env.ResolvedAppID != appID1 {
		t.Fatalf("expected resolved_app_id %q, got %v", appID1, env.ResolvedAppID)
	}

	var sawFlaggedWinner, sawUnflaggedApp2 bool
	for _, c := range env.Candidates {
		if c.AppID == appID1 && c.ExpectsReply {
			sawFlaggedWinner = true
		}
		if c.AppID == appID2 && !c.ExpectsReply {
			sawUnflaggedApp2 = true
		}
	}
	if !sawFlaggedWinner {
		t.Error("expected app1 candidate flagged expects_reply=true")
	}
	if !sawUnflaggedApp2 {
		t.Error("expected app2 candidate present with expects_reply=false")
	}
}

func TestAttribution_Ambiguous_TwoFlaggedApps(t *testing.T) {
	loadEnv()

	appSecret := os.Getenv("META_APP_SECRET")
	if appSecret == "" {
		t.Skip("META_APP_SECRET not set")
	}
	to := os.Getenv("TEST_RECIPIENT")
	if to == "" {
		t.Fatal("TEST_RECIPIENT env var is required")
	}

	resetCallbacks(t, messageReceiverURL)

	respA := sendMessageWithHeaders(t, app1Key, to, map[string]string{"X-Proxy-Expects-Reply": "true"})
	defer respA.Body.Close()
	if respA.StatusCode != http.StatusOK {
		t.Fatalf("app1 send: expected 200, got %d", respA.StatusCode)
	}

	respB := sendMessageWithHeaders(t, app2Key, to, map[string]string{"X-Proxy-Expects-Reply": "true"})
	defer respB.Body.Close()
	if respB.StatusCode != http.StatusOK {
		t.Fatalf("app2 send: expected 200, got %d", respB.StatusCode)
	}

	payload := []byte(fmt.Sprintf(
		`{"object":"whatsapp_business_account","entry":[{"changes":[{"value":{"contacts":[{"wa_id":"%s"}],"messages":[{"id":"test-ambiguous-%s","from":"%s","type":"text","text":{"body":"ok"}}]}}]}]}`,
		to, to, to,
	))
	postWebhook(t, appSecret, payload)

	env := waitForEnvelope(t, messageReceiverURL, func(e envelope) bool {
		return e.Attribution != ""
	}, 10*time.Second)

	if env.Attribution != "ambiguous" {
		t.Fatalf("expected attribution 'ambiguous', got %q", env.Attribution)
	}
	if env.ResolvedAppID != nil {
		t.Fatalf("expected nil resolved_app_id, got %q", *env.ResolvedAppID)
	}

	seen := map[string]bool{}
	for _, c := range env.Candidates {
		seen[c.AppID] = true
	}
	if !seen[appID1] || !seen[appID2] {
		t.Fatalf("expected both apps in candidates, got %+v", env.Candidates)
	}
	if len(env.Candidates) < 2 || !env.Candidates[0].ExpectsReply {
		t.Fatalf("expected flagged candidates first, got %+v", env.Candidates)
	}
}

// postWebhook signs and POSTs a raw Meta webhook payload to the proxy.
func postWebhook(t *testing.T, appSecret string, payload []byte) {
	t.Helper()
	sig := computeHMAC(payload, appSecret)
	req, _ := http.NewRequest(http.MethodPost, proxyURL()+"/webhook", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256="+sig)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post webhook: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /webhook, got %d", resp.StatusCode)
	}
}

// waitForEnvelope polls the receiver until a callback parses as an envelope
// matching pred, or fails after timeout.
func waitForEnvelope(t *testing.T, receiverURL string, pred func(envelope) bool, timeout time.Duration) envelope {
	t.Helper()
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		for _, raw := range fetchCallbacks(t, receiverURL) {
			var env envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				continue
			}
			if pred(env) {
				return env
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	t.Fatalf("timed out after %s waiting for matching envelope on %s", timeout, receiverURL)
	return envelope{}
}

// pinConversation calls POST /v1/conversations/{wa_id}/pin, authenticated as apiKey.
func pinConversation(t *testing.T, apiKey, waID string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, proxyURL()+"/v1/conversations/"+waID+"/pin", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("pin conversation: %v", err)
	}
	return resp
}

// unpinConversation calls DELETE /v1/conversations/{wa_id}/pin.
func unpinConversation(t *testing.T, apiKey, waID string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, proxyURL()+"/v1/conversations/"+waID+"/pin", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unpin conversation: %v", err)
	}
	return resp
}

func syntheticInboundPayload(waID, messageID, text string) []byte {
	return []byte(fmt.Sprintf(
		`{"object":"whatsapp_business_account","entry":[{"changes":[{"value":{"contacts":[{"wa_id":"%s"}],"messages":[{"id":"%s","from":"%s","type":"text","text":{"body":"%s"}}]}}]}]}`,
		waID, messageID, waID, text,
	))
}

func TestAttribution_Pinned(t *testing.T) {
	loadEnv()

	appSecret := os.Getenv("META_APP_SECRET")
	if appSecret == "" {
		t.Skip("META_APP_SECRET not set")
	}

	const waID = "pin-test-basic-wa-id"
	resetCallbacks(t, messageReceiverURL)

	resp := pinConversation(t, app2Key, waID)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 pinning conversation, got %d", resp.StatusCode)
	}
	t.Cleanup(func() { unpinConversation(t, app2Key, waID).Body.Close() })

	postWebhook(t, appSecret, syntheticInboundPayload(waID, "test-pinned-1", "yes"))

	env := waitForEnvelope(t, messageReceiverURL, func(e envelope) bool {
		return e.Attribution != ""
	}, 10*time.Second)

	if env.Attribution != "pinned" {
		t.Fatalf("expected attribution 'pinned', got %q", env.Attribution)
	}
	if env.ResolvedAppID == nil || *env.ResolvedAppID != appID2 {
		t.Fatalf("expected resolved_app_id %q, got %v", appID2, env.ResolvedAppID)
	}
}

func TestAttribution_UnpinFallsThroughToUnknown(t *testing.T) {
	loadEnv()

	appSecret := os.Getenv("META_APP_SECRET")
	if appSecret == "" {
		t.Skip("META_APP_SECRET not set")
	}

	const waID = "pin-test-unpin-wa-id"
	resetCallbacks(t, messageReceiverURL)

	resp := pinConversation(t, app1Key, waID)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 pinning conversation, got %d", resp.StatusCode)
	}

	del := unpinConversation(t, app1Key, waID)
	defer del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 unpinning conversation, got %d", del.StatusCode)
	}

	postWebhook(t, appSecret, syntheticInboundPayload(waID, "test-unpin-1", "hello"))

	env := waitForEnvelope(t, messageReceiverURL, func(e envelope) bool {
		return e.Attribution != ""
	}, 10*time.Second)

	if env.Attribution != "unknown" {
		t.Fatalf("expected attribution 'unknown' after unpin, got %q", env.Attribution)
	}
}

func TestAttribution_OutboundOverwritesExistingPin(t *testing.T) {
	loadEnv()

	appSecret := os.Getenv("META_APP_SECRET")
	if appSecret == "" {
		t.Skip("META_APP_SECRET not set")
	}
	to := os.Getenv("TEST_RECIPIENT")
	if to == "" {
		t.Fatal("TEST_RECIPIENT env var is required")
	}

	// Cleanup first so a leftover pin from a previous run never leaks into
	// this or later tests that share the same real recipient number.
	t.Cleanup(func() { unpinConversation(t, app1Key, to).Body.Close() })

	resetCallbacks(t, messageReceiverURL)

	pinResp := pinConversation(t, app2Key, to)
	pinResp.Body.Close()
	if pinResp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 pinning conversation, got %d", pinResp.StatusCode)
	}

	// App1 sends — pin-on-send must overwrite app2's pin.
	sendResp := sendMessage(t, app1Key, to)
	defer sendResp.Body.Close()
	if sendResp.StatusCode != http.StatusOK {
		t.Fatalf("app1 send: expected 200, got %d", sendResp.StatusCode)
	}

	postWebhook(t, appSecret, syntheticInboundPayload(to, "test-overwrite-1", "yes"))

	env := waitForEnvelope(t, messageReceiverURL, func(e envelope) bool {
		return e.Attribution != ""
	}, 10*time.Second)

	if env.Attribution != "pinned" {
		t.Fatalf("expected attribution 'pinned', got %q", env.Attribution)
	}
	if env.ResolvedAppID == nil || *env.ResolvedAppID != appID1 {
		t.Fatalf("expected resolved_app_id %q (the newest sender), got %v", appID1, env.ResolvedAppID)
	}
}
