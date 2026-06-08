package integration_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/joho/godotenv"
)

const (
	app1Key     = "307856bc-45c9-40ff-92ac-26ec1a8bb693"
	app1Receive = "http://localhost:9091"

	app2Key     = "257f51d1-24ab-4981-ad5c-9a2a2cf22b03"
	app2Receive = "http://localhost:9092"

	rateLimitKey        = "f1e2d3c4-b5a6-4789-8abc-def012345678"
	messageReceiverURL  = "http://localhost:9093"
)

func TestSendMessage_ReceivesSentStatus(t *testing.T) {
	loadEnv()

	to := os.Getenv("TEST_RECIPIENT")
	if to == "" {
		t.Fatal("TEST_RECIPIENT env var is required")
	}

	// Clear any callbacks left over from previous runs.
	resetCallbacks(t, app1Receive)

	resp := sendMessage(t, app1Key, to)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if len(result.Messages) == 0 {
		t.Fatal("no message_id in response")
	}
	t.Logf("message sent: %s", result.Messages[0].ID)

	t.Log("waiting for 'sent' status callback...")
	pollForStatus(t, app1Receive, "sent", 30*time.Second)
}

func TestMultiAppRouting(t *testing.T) {
	loadEnv()

	to := os.Getenv("TEST_RECIPIENT")
	if to == "" {
		t.Fatal("TEST_RECIPIENT env var is required")
	}

	resetCallbacks(t, app1Receive)
	resetCallbacks(t, app2Receive)

	resp1 := sendMessage(t, app1Key, to)
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp1.Body)
		t.Fatalf("app1 send: expected 200, got %d: %s", resp1.StatusCode, body)
	}
	msgID1 := parseMessageID(t, resp1)
	t.Logf("app1 message sent: %s", msgID1)

	resp2 := sendMessage(t, app2Key, to)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("app2 send: expected 200, got %d: %s", resp2.StatusCode, body)
	}
	msgID2 := parseMessageID(t, resp2)
	t.Logf("app2 message sent: %s", msgID2)

	t.Log("waiting for app1 'sent' status...")
	pollForMessageStatus(t, app1Receive, msgID1, "sent", 30*time.Second)

	t.Log("waiting for app2 'sent' status...")
	pollForMessageStatus(t, app2Receive, msgID2, "sent", 30*time.Second)

	// Routing isolation: neither receiver should have the other's message.
	for _, cb := range fetchCallbacks(t, app1Receive) {
		if findStatusByID(cb, msgID2, "") {
			t.Errorf("routing leak: app1 receiver got callback for app2 message %s", msgID2)
		}
	}
	for _, cb := range fetchCallbacks(t, app2Receive) {
		if findStatusByID(cb, msgID1, "") {
			t.Errorf("routing leak: app2 receiver got callback for app1 message %s", msgID1)
		}
	}
}

// --- auth tests ---

func TestInvalidAPIKey_Returns401(t *testing.T) {
	resp := sendMessage(t, "not-a-real-key", "5500000000000")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestMissingAPIKey_Returns401(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, proxyURL()+"/v1/messages",
		bytes.NewBufferString(`{"messaging_product":"whatsapp"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

// --- webhook endpoint tests ---

func TestWebhookVerification(t *testing.T) {
	loadEnv()
	verifyToken := os.Getenv("META_VERIFY_TOKEN")
	challenge := "test-challenge-12345"

	u := fmt.Sprintf("%s/webhook?hub.mode=subscribe&hub.verify_token=%s&hub.challenge=%s",
		proxyURL(), verifyToken, challenge)
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != challenge {
		t.Fatalf("expected challenge %q, got %q", challenge, string(body))
	}
}

func TestWebhookInvalidToken_Returns403(t *testing.T) {
	u := fmt.Sprintf("%s/webhook?hub.mode=subscribe&hub.verify_token=wrong-token&hub.challenge=abc",
		proxyURL())
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}

func TestWebhookInvalidSignature_Returns403(t *testing.T) {
	body := []byte(`{"object":"whatsapp_business_account","entry":[]}`)
	req, _ := http.NewRequest(http.MethodPost, proxyURL()+"/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256=0000000000000000000000000000000000000000000000000000000000000000")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}

// --- rate limit test ---

func TestRateLimit_Returns429(t *testing.T) {
	// integration-ratelimit app has rate: 2 — third request within a minute must be 429.
	for i := 1; i <= 2; i++ {
		resp := sendMessage(t, rateLimitKey, "5500000000000")
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("request %d should not be rate limited (rate=2)", i)
		}
	}

	resp := sendMessage(t, rateLimitKey, "5500000000000")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on third request, got %d", resp.StatusCode)
	}
}

// --- message receiver test ---

func TestInboundMessage_RoutedToMessageReceiver(t *testing.T) {
	loadEnv()

	appSecret := os.Getenv("META_APP_SECRET")
	if appSecret == "" {
		t.Skip("META_APP_SECRET not set")
	}

	resetCallbacks(t, messageReceiverURL)

	payload := []byte(`{"object":"whatsapp_business_account","entry":[{"changes":[{"value":{"messages":[{"id":"test-inbound-id","from":"5511999999999","type":"text","text":{"body":"hello"}}]}}]}]}`)

	sig := computeHMAC(payload, appSecret)
	req, _ := http.NewRequest(http.MethodPost, proxyURL()+"/webhook", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256="+sig)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	t.Log("waiting for inbound message to arrive at message_receiver...")
	waitForAnyCallback(t, messageReceiverURL, 10*time.Second)
}

// --- delivered status test ---

func TestSendMessage_ReceivesDeliveredStatus(t *testing.T) {
	loadEnv()

	to := os.Getenv("TEST_RECIPIENT")
	if to == "" {
		t.Fatal("TEST_RECIPIENT env var is required")
	}

	resetCallbacks(t, app1Receive)

	resp := sendMessage(t, app1Key, to)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	msgID := parseMessageID(t, resp)
	t.Logf("message sent: %s", msgID)

	t.Log("waiting for 'sent' status...")
	pollForMessageStatus(t, app1Receive, msgID, "sent", 30*time.Second)

	t.Log("waiting for 'delivered' status...")
	pollForMessageStatus(t, app1Receive, msgID, "delivered", 60*time.Second)
}

// --- helpers ---

func loadEnv() {
	godotenv.Load("../.env") //nolint:errcheck
}

func proxyURL() string {
	if u := os.Getenv("PROXY_URL"); u != "" {
		return u
	}
	return "http://localhost:8080"
}

func sendMessage(t *testing.T, apiKey, to string) *http.Response {
	t.Helper()
	body := fmt.Sprintf(
		`{"messaging_product":"whatsapp","to":"%s","type":"text","text":{"body":"integration test"}}`,
		to,
	)
	req, _ := http.NewRequest(http.MethodPost, proxyURL()+"/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send message: %v", err)
	}
	return resp
}

// resetCallbacks clears the receiver's stored callbacks.
func resetCallbacks(t *testing.T, receiverURL string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, receiverURL+"/callbacks", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("reset callbacks: %v", err)
	}
	resp.Body.Close()
}

// pollForStatus polls GET /callbacks until it finds a payload containing the
// given status value, or fails after timeout.
func pollForStatus(t *testing.T, receiverURL, status string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		callbacks := fetchCallbacks(t, receiverURL)

		for _, raw := range callbacks {
			if findStatus(raw, status) {
				t.Logf("received '%s' status", status)
				return
			}
		}

		time.Sleep(500 * time.Millisecond)
	}

	t.Fatalf("timed out after %s waiting for '%s' status", timeout, status)
}

// fetchCallbacks retrieves all stored callbacks from the receiver.
func fetchCallbacks(t *testing.T, receiverURL string) []json.RawMessage {
	t.Helper()
	resp, err := http.Get(receiverURL + "/callbacks")
	if err != nil {
		t.Fatalf("fetch callbacks: %v", err)
	}
	defer resp.Body.Close()

	var callbacks []json.RawMessage
	json.NewDecoder(resp.Body).Decode(&callbacks)
	return callbacks
}

// findStatus returns true if the payload contains a status entry matching the given value.
func findStatus(payload json.RawMessage, want string) bool {
	var body struct {
		Entry []struct {
			Changes []struct {
				Value struct {
					Statuses []struct {
						Status string `json:"status"`
					} `json:"statuses"`
				} `json:"value"`
			} `json:"changes"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return false
	}
	for _, entry := range body.Entry {
		for _, change := range entry.Changes {
			for _, s := range change.Value.Statuses {
				if s.Status == want {
					return true
				}
			}
		}
	}
	return false
}

// parseMessageID extracts the first message ID from a /v1/messages response.
func parseMessageID(t *testing.T, resp *http.Response) string {
	t.Helper()
	var result struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if len(result.Messages) == 0 {
		t.Fatal("no message_id in response")
	}
	return result.Messages[0].ID
}

// pollForMessageStatus polls until a callback arrives for the given message ID and status.
func pollForMessageStatus(t *testing.T, receiverURL, messageID, status string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, raw := range fetchCallbacks(t, receiverURL) {
			if findStatusByID(raw, messageID, status) {
				t.Logf("received '%s' status for message %s", status, messageID)
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for '%s' status for message %s", timeout, status, messageID)
}

// computeHMAC returns the hex-encoded HMAC-SHA256 of body signed with secret.
func computeHMAC(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// waitForAnyCallback polls until at least one callback is stored in the receiver.
func waitForAnyCallback(t *testing.T, receiverURL string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cbs := fetchCallbacks(t, receiverURL); len(cbs) > 0 {
			t.Logf("message_receiver got %d callback(s)", len(cbs))
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for any callback on %s", timeout, receiverURL)
}

// findStatusByID returns true if the payload contains a status entry matching messageID.
// If wantStatus is non-empty it must also match the status field.
func findStatusByID(payload json.RawMessage, messageID, wantStatus string) bool {
	var body struct {
		Entry []struct {
			Changes []struct {
				Value struct {
					Statuses []struct {
						ID     string `json:"id"`
						Status string `json:"status"`
					} `json:"statuses"`
				} `json:"value"`
			} `json:"changes"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return false
	}
	for _, entry := range body.Entry {
		for _, change := range entry.Changes {
			for _, s := range change.Value.Statuses {
				if s.ID == messageID && (wantStatus == "" || s.Status == wantStatus) {
					return true
				}
			}
		}
	}
	return false
}
