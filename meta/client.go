package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const ProductionBaseURL = "https://graph.facebook.com"

type Client struct {
	httpClient    *http.Client
	accessToken   string
	phoneNumberID string
	apiVersion    string
	BaseURL       string
	log           *slog.Logger
}

func NewClient(accessToken, phoneNumberID, apiVersion string, log *slog.Logger) *Client {
	return &Client{
		httpClient:    &http.Client{Timeout: 30 * time.Second},
		accessToken:   accessToken,
		phoneNumberID: phoneNumberID,
		apiVersion:    apiVersion,
		BaseURL:       ProductionBaseURL,
		log:           log,
	}
}

// SendResponse is the relevant portion of Meta's send message response.
type SendResponse struct {
	MessagingProduct string    `json:"messaging_product"`
	Contacts         []Contact `json:"contacts"`
	Messages         []Message `json:"messages"`
}

type Contact struct {
	Input string `json:"input"`
	WaID  string `json:"wa_id"`
}

type Message struct {
	ID            string `json:"id"`
	MessageStatus string `json:"message_status,omitempty"`
}

// SendMessage forwards a raw message payload to the Meta Cloud API.
// body must be a valid JSON object matching Meta's send message schema.
func (c *Client) SendMessage(ctx context.Context, body json.RawMessage) (*SendResponse, error) {
	url := fmt.Sprintf("%s/%s/%s/messages", c.BaseURL, c.apiVersion, c.phoneNumberID)

	c.log.Debug("meta: sending message", "url", url, "body_size", len(body))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.accessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("meta request: %w", err)
	}
	defer resp.Body.Close()

	c.log.Debug("meta: response received", "status", resp.StatusCode)

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read meta response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("meta error %d: %s", resp.StatusCode, respBody)
	}

	var result SendResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode meta response: %w", err)
	}

	c.log.Debug("meta: message accepted", "message_count", len(result.Messages))

	return &result, nil
}
