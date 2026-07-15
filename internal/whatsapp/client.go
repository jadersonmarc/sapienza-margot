package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Sender delivers an outbound message and returns its provider id. The pipeline
// depends on this seam; production uses *Client (Evolution), tests use *MockSender.
type Sender interface {
	SendText(ctx context.Context, instance, number, text string) (string, error)
}

// Client sends outbound messages through the Evolution API (copied from
// rag-agente-go/internal/whatsapp/client.go).
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewClient builds an Evolution client for the given base URL and API key.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

type sendTextRequest struct {
	Number string `json:"number"`
	Text   string `json:"text"`
}

type sendTextResponse struct {
	Key struct {
		ID string `json:"id"`
	} `json:"key"`
}

// SendText posts a text message to a number via the given Evolution instance and
// returns the provider message id when available.
func (c *Client) SendText(ctx context.Context, instance, number, text string) (string, error) {
	body, err := json.Marshal(sendTextRequest{Number: number, Text: text})
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("%s/message/sendText/%s", c.baseURL, instance)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apikey", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("evolution sendText: status %d", resp.StatusCode)
	}

	var out sendTextResponse
	_ = json.NewDecoder(resp.Body).Decode(&out) // provider id is best-effort
	return out.Key.ID, nil
}

// SentMessage records one outbound call captured by MockSender.
type SentMessage struct {
	Instance string
	Number   string
	Text     string
}

// MockSender captures outbound messages instead of calling Evolution — used in
// tests so no real WhatsApp/Evolution server is needed.
type MockSender struct {
	mu   sync.Mutex
	Sent []SentMessage
}

// SendText records the message and returns a deterministic fake provider id.
func (m *MockSender) SendText(_ context.Context, instance, number, text string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Sent = append(m.Sent, SentMessage{Instance: instance, Number: number, Text: text})
	return fmt.Sprintf("mock-%d", len(m.Sent)), nil
}

// Messages returns a copy of the captured outbound messages.
func (m *MockSender) Messages() []SentMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SentMessage, len(m.Sent))
	copy(out, m.Sent)
	return out
}
