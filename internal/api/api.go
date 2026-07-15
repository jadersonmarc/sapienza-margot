// Package api is Margot's product API, consumed by the console BFF. Auth is the
// short-lived JWT the core issues (validated by kit/authclient); every request
// is scoped to the JWT's tenant, and conversation data is read/written under
// kit/tenancy.WithTenant — no cross-tenant access.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/authclient"
	"github.com/jadersonmarc/sapienza-kit/gating"
	"github.com/jadersonmarc/sapienza-kit/tenancy"

	"github.com/jadersonmarc/sapienza-margot/internal/secrets"
	"github.com/jadersonmarc/sapienza-margot/internal/store"
	"github.com/jadersonmarc/sapienza-margot/internal/whatsapp"
)

const produto = "margot"

// Server holds the API dependencies.
type Server struct {
	pool     *pgxpool.Pool
	verifier *authclient.Verifier
	gate     *gating.Client
	sender   whatsapp.Sender
	cipher   *secrets.Cipher
}

// NewServer builds the API server.
func NewServer(pool *pgxpool.Pool, verifier *authclient.Verifier, gate *gating.Client, sender whatsapp.Sender, cipher *secrets.Cipher) *Server {
	return &Server{pool: pool, verifier: verifier, gate: gate, sender: sender, cipher: cipher}
}

// Handler returns the mux for the /api/v1 surface.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/conversations", s.authed(s.listConversations))
	mux.HandleFunc("GET /api/v1/conversations/{id}/messages", s.authed(s.listMessages))
	mux.HandleFunc("POST /api/v1/conversations/{id}/send", s.authed(s.sendMessage))
	mux.HandleFunc("POST /api/v1/conversations/{id}/handoff", s.authed(s.handoff))
	mux.HandleFunc("GET /api/v1/config", s.authed(s.getConfig))
	mux.HandleFunc("PUT /api/v1/config", s.authed(s.putConfig))
	mux.HandleFunc("GET /api/v1/setup", s.authed(s.getSetup))
	return mux
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID)

// authed validates the core JWT (produto must be margot or unset) and injects
// the tenant id. No valid token → 401.
func (s *Server) authed(fn handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if tok == "" {
			writeErr(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		claims, err := s.verifier.Verify(tok)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "invalid token")
			return
		}
		if claims.Produto != "" && claims.Produto != produto {
			writeErr(w, http.StatusForbidden, "token not scoped to margot")
			return
		}
		fn(w, r, claims.TenantID)
	}
}

// withTenant runs fn in a transaction scoped to the tenant's schema.
func (s *Server) withTenant(ctx context.Context, tenantID uuid.UUID, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tenancy.WithTenant(ctx, tx, tenantID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ── handlers ─────────────────────────────────────────────────────────────────

func (s *Server) listConversations(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID) {
	var out []store.ConversationView
	if err := s.withTenant(r.Context(), tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = store.ListConversations(r.Context(), tx, 100)
		return err
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversations": out})
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID) {
	convID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid conversation id")
		return
	}
	var out []store.Message
	if err := s.withTenant(r.Context(), tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = store.ListRecentMessages(r.Context(), tx, convID, 200)
		return err
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": out})
}

type sendReq struct {
	Text string `json:"text"`
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID) {
	convID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid conversation id")
		return
	}
	var body sendReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil || strings.TrimSpace(body.Text) == "" {
		writeErr(w, http.StatusBadRequest, "text required")
		return
	}
	if ok, err := s.gate.TenantCanOperate(r.Context(), tenantID, produto); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	} else if !ok {
		writeErr(w, http.StatusForbidden, "subscription not active")
		return
	}

	// Resolve the conversation's contact phone + channel instance, send, record.
	var phone, instance string
	if err := s.withTenant(r.Context(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`SELECT ct.phone FROM conversations c JOIN contacts ct ON ct.id = c.contact_id WHERE c.id = $1`, convID,
		).Scan(&phone)
	}); err != nil {
		writeErr(w, http.StatusNotFound, "conversation not found")
		return
	}
	if err := s.pool.QueryRow(r.Context(),
		`SELECT evolution_instance FROM margot.tenant_channels WHERE tenant_id = $1`, tenantID,
	).Scan(&instance); err != nil {
		writeErr(w, http.StatusBadRequest, "channel not configured")
		return
	}
	sentID, err := s.sender.SendText(r.Context(), instance, phone, body.Text)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "send failed")
		return
	}
	if err := s.withTenant(r.Context(), tenantID, func(tx pgx.Tx) error {
		_, err := store.InsertMessage(r.Context(), tx, store.Message{
			ConversationID: convID, Direction: "out", Sender: "human",
			Content: body.Text, ProviderID: &sentID,
		})
		return err
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider_id": sentID})
}

func (s *Server) handoff(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID) {
	convID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid conversation id")
		return
	}
	if err := s.withTenant(r.Context(), tenantID, func(tx pgx.Tx) error {
		if err := store.InsertHandoff(r.Context(), tx, convID, "manual"); err != nil {
			return err
		}
		return store.SetConversationMode(r.Context(), tx, convID, "human")
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type configDTO struct {
	SystemPrompt string `json:"system_prompt"`
	Tone         string `json:"tone"`
	Fallback     string `json:"fallback"`
	MaxTokens    int32  `json:"max_tokens"`
	AIModel      string `json:"ai_model"`
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID) {
	var c configDTO
	err := s.pool.QueryRow(r.Context(),
		`SELECT system_prompt, tone, fallback, max_tokens, ai_model
		   FROM margot.tenant_channels WHERE tenant_id = $1`, tenantID,
	).Scan(&c.SystemPrompt, &c.Tone, &c.Fallback, &c.MaxTokens, &c.AIModel)
	if err == pgx.ErrNoRows {
		writeErr(w, http.StatusNotFound, "channel not configured")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) putConfig(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID) {
	var c configDTO
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&c); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	tag, err := s.pool.Exec(r.Context(), `
		UPDATE margot.tenant_channels
		   SET system_prompt = $2, tone = $3, fallback = $4, max_tokens = $5, ai_model = $6, updated_at = now()
		 WHERE tenant_id = $1`,
		tenantID, c.SystemPrompt, c.Tone, c.Fallback, c.MaxTokens, c.AIModel)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, http.StatusNotFound, "channel not configured")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// getSetup reports onboarding status so the console can guide the client on what
// the subscription needs (channel connected, agent configured, subscription active).
func (s *Server) getSetup(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID) {
	var instance, systemPrompt string
	channelOK := true
	err := s.pool.QueryRow(r.Context(),
		`SELECT COALESCE(evolution_instance, ''), system_prompt FROM margot.tenant_channels WHERE tenant_id = $1`, tenantID,
	).Scan(&instance, &systemPrompt)
	if err == pgx.ErrNoRows {
		channelOK = false
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	active, err := s.gate.TenantCanOperate(r.Context(), tenantID, produto)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"channel_connected":   channelOK && instance != "",
		"agent_configured":    systemPrompt != "",
		"subscription_active": active,
	})
}

// ── helpers ──────────────────────────────────────────────────────────────────

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
