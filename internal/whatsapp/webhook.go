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
	if !h.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
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

	if err := h.processor.Process(r.Context(), ch, in); err != nil {
		log.Printf("whatsapp: process inbound (tenant %s): %v", ch.TenantID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// authorized compares the `apikey` header to the configured webhook secret in
// constant time. An empty secret disables the check (dev only).
func (h *Handler) authorized(r *http.Request) bool {
	if h.secret == "" {
		return true
	}
	got := r.Header.Get("apikey")
	return subtle.ConstantTimeCompare([]byte(got), []byte(h.secret)) == 1
}
