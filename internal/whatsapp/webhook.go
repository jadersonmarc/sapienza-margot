package whatsapp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"

	"github.com/jadersonmarc/sapienza-margot/internal/channel"
)

// Resolver maps an Evolution instance to its tenant channel.
type Resolver interface {
	ByInstance(ctx context.Context, instance string) (channel.TenantChannel, error)
}

// Processor handles a normalized inbound message for a resolved tenant channel.
// Implemented by internal/pipeline; kept here so the transport stays decoupled
// from the conversation logic.
type Processor interface {
	Process(ctx context.Context, ch channel.TenantChannel, in Inbound) error
}

// Handler receives Evolution webhooks: authenticates, parses, resolves the
// tenant by instance, and delegates the actionable inbound to the Processor.
type Handler struct {
	resolver  Resolver
	processor Processor
	secret    string
}

// NewHandler builds the webhook handler.
func NewHandler(resolver Resolver, processor Processor, secret string) *Handler {
	return &Handler{resolver: resolver, processor: processor, secret: secret}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB cap to avoid DoS
	var payload evolutionWebhook
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	// Only act on inbound message upserts; acknowledge everything else.
	if payload.Event != "" && payload.Event != "messages.upsert" {
		w.WriteHeader(http.StatusOK)
		return
	}
	in, ok := parseInbound(payload)
	if !ok || in.FromMe || in.Text == "" {
		w.WriteHeader(http.StatusOK) // not actionable
		return
	}

	ch, err := h.resolver.ByInstance(r.Context(), in.Instance)
	if err != nil {
		log.Printf("whatsapp: resolve instance %q: %v", in.Instance, err)
		w.WriteHeader(http.StatusOK) // unknown instance: ack to stop retries
		return
	}

	// Authorize AFTER resolving: the secret is per tenant, so there is nothing to
	// compare against until we know which channel this claims to be. Nothing
	// expensive has run yet — parsing is cheap and bounded, and the model call is
	// downstream in Process.
	if !h.authorized(r, ch) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if err := h.processor.Process(r.Context(), ch, in); err != nil {
		log.Printf("whatsapp: process inbound (tenant %s): %v", ch.TenantID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// authorized compares the `apikey` header to the channel's secret in constant
// time, falling back to the global one while a tenant has none of its own.
//
// Per-tenant is the point: with a single shared secret, anyone holding it could
// forge a payload naming any instance — injecting messages into that tenant's
// schema and spending our model budget with no revenue behind it. Rotate a
// tenant's secret via POST /api/v1/channel/rotate-webhook-secret.
//
// Fail-closed: with neither secret configured we reject. This used to return true
// ("dev only"), which meant a missing env in production silently opened the
// endpoint to the world.
func (h *Handler) authorized(r *http.Request, ch channel.TenantChannel) bool {
	expected := ch.WebhookSecret
	if expected == "" {
		expected = h.secret
	}
	if expected == "" {
		return false
	}
	got := r.Header.Get("apikey")
	return subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
}
