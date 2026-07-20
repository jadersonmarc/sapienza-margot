// Package api is Margot's product API, consumed by the console BFF. Auth is the
// short-lived JWT the core issues (validated by kit/authclient); every request
// is scoped to the JWT's tenant, and conversation data is read/written under
// kit/tenancy.WithTenant — no cross-tenant access.
package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/authclient"
	"github.com/jadersonmarc/sapienza-kit/gating"
	"github.com/jadersonmarc/sapienza-kit/tenancy"

	"github.com/jadersonmarc/sapienza-margot/internal/secrets"
	"github.com/jadersonmarc/sapienza-margot/internal/store"
	"github.com/jadersonmarc/sapienza-margot/internal/whatsapp"
)

const produto = "margot"

// Invalidator drops a cached channel so a config change takes effect now instead
// of after the resolver's TTL. Satisfied by *channel.Resolver; an interface keeps
// the API from depending on the resolver's construction.
type Invalidator interface {
	Invalidate(instance string)
}

// Server holds the API dependencies.
type Server struct {
	pool     *pgxpool.Pool
	verifier *authclient.Verifier
	gate     *gating.Client
	drivers  *whatsapp.Registry
	cipher   *secrets.Cipher
	cache    Invalidator // may be nil (tests that don't resolve channels)
}

// NewServer builds the API server.
func NewServer(pool *pgxpool.Pool, verifier *authclient.Verifier, gate *gating.Client, drivers *whatsapp.Registry, cipher *secrets.Cipher, cache Invalidator) *Server {
	return &Server{pool: pool, verifier: verifier, gate: gate, drivers: drivers, cipher: cipher, cache: cache}
}

// Handler returns the mux for the /api/v1 surface.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/conversations", s.authed(s.listConversations))
	mux.HandleFunc("GET /api/v1/conversations/{id}/messages", s.authed(s.listMessages))
	mux.HandleFunc("POST /api/v1/conversations/{id}/send", s.authed(s.sendMessage))
	mux.HandleFunc("POST /api/v1/conversations/{id}/handoff", s.authed(s.handoff))
	mux.HandleFunc("GET /api/v1/config", s.authed(s.getConfig))
	mux.HandleFunc("PUT /api/v1/config", s.authedManager(s.putConfig))
	mux.HandleFunc("GET /api/v1/setup", s.authed(s.getSetup))
	mux.HandleFunc("PUT /api/v1/channel", s.authedManager(s.putChannel))
	mux.HandleFunc("POST /api/v1/channel/rotate-webhook-secret", s.authedManager(s.rotateWebhookSecret))
	return mux
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID)

// authed validates the core JWT and injects the tenant id. No valid token → 401.
// The token must be scoped to margot (produto == "margot"); um token sem a claim
// não passa mais (antes `!= "" && != produto` deixava passar sem escopo).
func (s *Server) authed(fn handlerFunc) http.HandlerFunc {
	return s.guard(fn, false)
}

// authedManager is authed + requires an elevated role (owner|admin): writes de
// config/canal não são para qualquer membro do tenant. Superadmin do core chega
// achatado em "owner", então passa.
func (s *Server) authedManager(fn handlerFunc) http.HandlerFunc {
	return s.guard(fn, true)
}

func (s *Server) guard(fn handlerFunc, requireManager bool) http.HandlerFunc {
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
		if claims.Produto != produto {
			writeErr(w, http.StatusForbidden, "token not scoped to margot")
			return
		}
		if requireManager && claims.Role != "owner" && claims.Role != "admin" {
			writeErr(w, http.StatusForbidden, "requer papel owner ou admin")
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

	// Resolve the conversation's contact phone + channel instance/driver, send, record.
	var phone, instance, driver string
	if err := s.withTenant(r.Context(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`SELECT ct.phone FROM conversations c JOIN contacts ct ON ct.id = c.contact_id WHERE c.id = $1`, convID,
		).Scan(&phone)
	}); err != nil {
		writeErr(w, http.StatusNotFound, "conversation not found")
		return
	}
	if err := s.pool.QueryRow(r.Context(),
		`SELECT evolution_instance, driver FROM margot.tenant_channels WHERE tenant_id = $1`, tenantID,
	).Scan(&instance, &driver); err != nil {
		writeErr(w, http.StatusBadRequest, "channel not configured")
		return
	}
	// A manual agent reply from the console is NOT billed (sender="human"); billing
	// is per AI "resposta" and happens in the pipeline.
	sentID, err := s.drivers.For(driver).SendText(r.Context(), instance, phone, body.Text)
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

type channelDTO struct {
	EvolutionInstance        string `json:"evolution_instance"`
	WhatsappNumber           string `json:"whatsapp_number"`
	Driver                   string `json:"driver"`
	DedicatedNumberConfirmed bool   `json:"dedicated_number_confirmed"`
}

// putChannel vincula (cria/atualiza) a identidade do canal: qual instância do
// Evolution roteia para este tenant. É o passo de onboarding que faltava — sem uma
// linha aqui, o console mostra "canal não provisionado". Só owner/admin (na
// prática, superadmin Sapienza; o gate duro fica no console). Os campos de agente
// nascem no default do schema; quem os edita depois é putConfig.
func (s *Server) putChannel(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID) {
	var c channelDTO
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&c); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	c.EvolutionInstance = strings.TrimSpace(c.EvolutionInstance)
	if c.EvolutionInstance == "" {
		writeErr(w, http.StatusBadRequest, "evolution_instance obrigatória")
		return
	}
	if c.Driver == "" {
		c.Driver = "evolution"
	}
	_, err := s.pool.Exec(r.Context(), `
		INSERT INTO margot.tenant_channels
		       (tenant_id, evolution_instance, whatsapp_number, driver, dedicated_number_confirmed)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5)
		ON CONFLICT (tenant_id) DO UPDATE
		   SET evolution_instance = EXCLUDED.evolution_instance,
		       whatsapp_number = EXCLUDED.whatsapp_number,
		       driver = EXCLUDED.driver,
		       dedicated_number_confirmed = EXCLUDED.dedicated_number_confirmed,
		       updated_at = now()`,
		tenantID, c.EvolutionInstance, c.WhatsappNumber, c.Driver, c.DedicatedNumberConfirmed)
	if err != nil {
		// evolution_instance é UNIQUE: outra tenant já usa essa instância.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			writeErr(w, http.StatusConflict, "instância já usada por outro tenant")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Roteamento/driver mudou: invalida o cache do resolver (senão até 60s de TTL).
	if s.cache != nil {
		s.cache.Invalidate(c.EvolutionInstance)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type configDTO struct {
	// Identidade do canal (vínculo) — read-only no getConfig; escrita via putChannel.
	EvolutionInstance string `json:"evolution_instance"`
	WhatsappNumber    string `json:"whatsapp_number"`
	// Comportamento do agente — escrita via putConfig.
	SystemPrompt             string `json:"system_prompt"`
	Tone                     string `json:"tone"`
	Fallback                 string `json:"fallback"`
	MaxTokens                int32  `json:"max_tokens"`
	AIModel                  string `json:"ai_model"`
	Driver                   string `json:"driver"`                     // "evolution" | "meta"
	DedicatedNumberConfirmed bool   `json:"dedicated_number_confirmed"` // onboarding requirement
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID) {
	var c configDTO
	err := s.pool.QueryRow(r.Context(),
		`SELECT COALESCE(evolution_instance, ''), COALESCE(whatsapp_number, ''),
		        system_prompt, tone, fallback, max_tokens, ai_model, driver, dedicated_number_confirmed
		   FROM margot.tenant_channels WHERE tenant_id = $1`, tenantID,
	).Scan(&c.EvolutionInstance, &c.WhatsappNumber,
		&c.SystemPrompt, &c.Tone, &c.Fallback, &c.MaxTokens, &c.AIModel, &c.Driver, &c.DedicatedNumberConfirmed)
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

// rotateWebhookSecret mints a fresh per-tenant webhook secret, stores it
// encrypted and returns it ONCE — it is not readable afterwards. Paste the value
// into the Evolution instance's webhook config (header `apikey`).
//
// Until a tenant has its own, the webhook falls back to the global secret; after
// rotating, only this value is accepted for this instance.
func (s *Server) rotateWebhookSecret(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID) {
	if s.cipher == nil {
		writeErr(w, http.StatusServiceUnavailable, "MARGOT_ENC_KEY não configurada: não é possível cifrar o segredo")
		return
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		writeErr(w, http.StatusInternalServerError, "falha ao gerar segredo")
		return
	}
	secret := base64.RawURLEncoding.EncodeToString(buf)
	enc, err := s.cipher.Encrypt(secret)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	var instance string
	err = s.pool.QueryRow(r.Context(), `
		UPDATE margot.tenant_channels
		   SET webhook_secret_enc = $2, updated_at = now()
		 WHERE tenant_id = $1
		 RETURNING evolution_instance`, tenantID, enc).Scan(&instance)
	if err == pgx.ErrNoRows {
		writeErr(w, http.StatusNotFound, "channel not configured")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.cache != nil {
		s.cache.Invalidate(instance) // sem isto o segredo novo só valeria após o TTL
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"instance": instance,
		"secret":   secret,
		"aviso":    "guarde agora: este valor não é exibido novamente. Configure-o no header `apikey` do webhook desta instância na Evolution.",
	})
}

// putConfig edita SÓ o comportamento do agente. Identidade e roteamento do canal
// (instância, número, driver, número dedicado) são do vínculo — putChannel,
// superadmin — para o cliente owner/admin não mexer no roteamento por aqui.
func (s *Server) putConfig(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID) {
	var c configDTO
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&c); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	tag, err := s.pool.Exec(r.Context(), `
		UPDATE margot.tenant_channels
		   SET system_prompt = $2, tone = $3, fallback = $4, max_tokens = $5, ai_model = $6,
		       updated_at = now()
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
	var instance, systemPrompt, driver string
	var dedicated bool
	channelOK := true
	err := s.pool.QueryRow(r.Context(),
		`SELECT COALESCE(evolution_instance, ''), system_prompt, driver, dedicated_number_confirmed
		   FROM margot.tenant_channels WHERE tenant_id = $1`, tenantID,
	).Scan(&instance, &systemPrompt, &driver, &dedicated)
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
	// The Evolution driver requires a confirmed dedicated number before it can be
	// considered connected (never the owner's personal/main line).
	connected := channelOK && instance != ""
	if driver == "evolution" {
		connected = connected && dedicated
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"channel_connected":          connected,
		"agent_configured":           systemPrompt != "",
		"subscription_active":        active,
		"driver":                     driver,
		"dedicated_number_confirmed": dedicated,
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
