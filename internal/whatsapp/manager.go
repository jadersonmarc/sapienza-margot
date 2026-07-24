package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Manager talks to the Evolution v2 instance-management API (create instance,
// fetch QR, read connection state). It uses the GLOBAL apikey — the same key the
// Evolution manager panel uses — so it can provision instances on the tenant's
// behalf. This is the seam that makes onboarding self-serve: the console never
// asks the subscriber for an instance name or webhook secret; the backend creates
// the instance with the webhook already wired and returns a QR code to scan.
//
// Separate from *Client (SendText): sending is per-tenant driver work; this is
// platform-level provisioning.
type Manager struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewManager builds an Evolution management client for the global API base/key.
func NewManager(baseURL, apiKey string) *Manager {
	return &Manager{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 20 * time.Second},
	}
}

// Configured reports whether the global Evolution API is set (URL + key).
func (m *Manager) Configured() bool { return m.baseURL != "" && m.apiKey != "" }

func (m *Manager) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var r *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	} else {
		r = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.baseURL+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apikey", m.apiKey)
	return m.http.Do(req)
}

// webhook config (Evolution v2). Eventos em UPPER_SNAKE; o payload entregue usa
// "messages.upsert". `Enabled` é obrigatório em v2 — sem ele o webhook fica
// registrado mas inativo (não dispara). ByEvents/Base64 completam o shape.
type evoWebhook struct {
	Enabled  bool              `json:"enabled"`
	URL      string            `json:"url"`
	Headers  map[string]string `json:"headers,omitempty"`
	ByEvents bool              `json:"byEvents"`
	Base64   bool              `json:"base64"`
	Events   []string          `json:"events"`
}

func webhookConfig(webhookURL, secret string) *evoWebhook {
	return &evoWebhook{
		Enabled:  true,
		URL:      webhookURL,
		Headers:  map[string]string{"apikey": secret},
		ByEvents: false,
		Base64:   false,
		Events:   []string{"MESSAGES_UPSERT"},
	}
}

type createInstanceRequest struct {
	InstanceName string      `json:"instanceName"`
	Integration  string      `json:"integration"`
	QRCode       bool        `json:"qrcode"`
	Webhook      *evoWebhook `json:"webhook,omitempty"`
}

// CreateInstance creates a Baileys instance with the webhook already wired to
// webhookURL (header `apikey: secret`). Idempotent from the caller's view: an
// instance that already exists (HTTP 403/409 with a "already in use" message) is
// treated as success, since the caller then fetches the QR via ConnectQR.
func (m *Manager) CreateInstance(ctx context.Context, name, webhookURL, secret string) error {
	resp, err := m.do(ctx, http.MethodPost, "/instance/create", createInstanceRequest{
		InstanceName: name,
		Integration:  "WHATSAPP-BAILEYS",
		QRCode:       true,
		Webhook:      webhookConfig(webhookURL, secret),
	})
	if err != nil {
		return fmt.Errorf("evolution create instance: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusMultipleChoices {
		return nil
	}
	// Already exists → not an error; the caller re-fetches the QR via connect.
	msg := readBodyLimited(resp)
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusConflict ||
		strings.Contains(strings.ToLower(msg), "already") || strings.Contains(strings.ToLower(msg), "in use") {
		return nil
	}
	return fmt.Errorf("evolution create instance: status %d: %s", resp.StatusCode, msg)
}

// SetWebhook (re)configura o webhook da instância explicitamente — garante o
// webhook ATIVO mesmo quando a versão do Evolution ignora o embutido no create.
// Idempotente. Evolution v2: POST /webhook/set/{instance} com { "webhook": {...} }.
func (m *Manager) SetWebhook(ctx context.Context, name, webhookURL, secret string) error {
	resp, err := m.do(ctx, http.MethodPost, "/webhook/set/"+name, struct {
		Webhook *evoWebhook `json:"webhook"`
	}{Webhook: webhookConfig(webhookURL, secret)})
	if err != nil {
		return fmt.Errorf("evolution set webhook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("evolution set webhook: status %d: %s", resp.StatusCode, readBodyLimited(resp))
	}
	return nil
}

// WebhookInfo é a config real do webhook de uma instância (diagnóstico).
type WebhookInfo struct {
	URL       string   `json:"url"`
	Enabled   bool     `json:"enabled"`
	Events    []string `json:"events"`
	HasAPIKey bool     `json:"has_apikey"`
}

// FindWebhook consulta a config real do webhook no Evolution (GET
// /webhook/find/{instance}). nil,nil se a instância/webhook não existe.
func (m *Manager) FindWebhook(ctx context.Context, name string) (*WebhookInfo, error) {
	resp, err := m.do(ctx, http.MethodGet, "/webhook/find/"+name, nil)
	if err != nil {
		return nil, fmt.Errorf("evolution find webhook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("evolution find webhook: status %d: %s", resp.StatusCode, readBodyLimited(resp))
	}
	var raw struct {
		URL     string          `json:"url"`
		Enabled bool            `json:"enabled"`
		Events  []string        `json:"events"`
		Headers json.RawMessage `json:"headers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("evolution find webhook decode: %w", err)
	}
	if raw.URL == "" {
		return nil, nil
	}
	h := string(raw.Headers)
	return &WebhookInfo{
		URL:       raw.URL,
		Enabled:   raw.Enabled,
		Events:    raw.Events,
		HasAPIKey: len(raw.Headers) > 0 && h != "null" && h != "{}" && strings.Contains(strings.ToLower(h), "apikey"),
	}, nil
}

// qrResponse covers both the create response (qrcode.base64) and the connect
// response, which may return the base64 at the top level depending on version.
type qrResponse struct {
	Base64 string `json:"base64"`
	Code   string `json:"code"`
	QRCode struct {
		Base64 string `json:"base64"`
		Code   string `json:"code"`
	} `json:"qrcode"`
}

func (q qrResponse) base64() string {
	if q.Base64 != "" {
		return q.Base64
	}
	return q.QRCode.Base64
}

// ConnectQR fetches the current QR code (base64 data URI) for an instance that is
// not yet connected. Empty base64 means the instance is already connected.
func (m *Manager) ConnectQR(ctx context.Context, name string) (string, error) {
	resp, err := m.do(ctx, http.MethodGet, "/instance/connect/"+name, nil)
	if err != nil {
		return "", fmt.Errorf("evolution connect: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("evolution connect: status %d: %s", resp.StatusCode, readBodyLimited(resp))
	}
	var q qrResponse
	if err := json.NewDecoder(resp.Body).Decode(&q); err != nil {
		return "", fmt.Errorf("evolution connect decode: %w", err)
	}
	return q.base64(), nil
}

type stateResponse struct {
	Instance struct {
		State    string `json:"state"`
		OwnerJID string `json:"ownerJid"`
		Number   string `json:"number"`
	} `json:"instance"`
	// some versions return state at the top level
	State string `json:"state"`
}

// State returns the connection state ("open"|"connecting"|"close") and, once
// connected, the connected number (best-effort, parsed from ownerJid).
func (m *Manager) State(ctx context.Context, name string) (state, number string, err error) {
	resp, err := m.do(ctx, http.MethodGet, "/instance/connectionState/"+name, nil)
	if err != nil {
		return "", "", fmt.Errorf("evolution state: %w", err)
	}
	defer resp.Body.Close()
	// Instância inexistente no Evolution (nunca criada, ou removida no painel): não
	// é erro — é "desconectado". Assim a tela de configuração mostra o reconectar
	// (que recria a instância) em vez de ficar presa num 502.
	if resp.StatusCode == http.StatusNotFound {
		return "close", "", nil
	}
	if resp.StatusCode >= http.StatusMultipleChoices {
		return "", "", fmt.Errorf("evolution state: status %d: %s", resp.StatusCode, readBodyLimited(resp))
	}
	var s stateResponse
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return "", "", fmt.Errorf("evolution state decode: %w", err)
	}
	state = s.Instance.State
	if state == "" {
		state = s.State
	}
	number = s.Instance.Number
	if number == "" && s.Instance.OwnerJID != "" {
		// ownerJid looks like "5521999999999@s.whatsapp.net"
		number = strings.SplitN(s.Instance.OwnerJID, "@", 2)[0]
	}
	return state, number, nil
}

func readBodyLimited(resp *http.Response) string {
	buf := make([]byte, 2048)
	n, _ := resp.Body.Read(buf)
	return string(buf[:n])
}
