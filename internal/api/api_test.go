package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/authclient"
	"github.com/jadersonmarc/sapienza-kit/gating"

	"github.com/jadersonmarc/sapienza-margot/internal/api"
	"github.com/jadersonmarc/sapienza-margot/internal/channel"
	"github.com/jadersonmarc/sapienza-margot/internal/pipeline"
	"github.com/jadersonmarc/sapienza-margot/internal/testutil"
	"github.com/jadersonmarc/sapienza-margot/internal/whatsapp"
)

var secret = []byte("test-product-jwt-secret")

func mint(t *testing.T, tid uuid.UUID) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"uid":     uuid.New().String(),
		"tid":     tid.String(),
		"produto": "margot",
		"iss":     "sapienza-core",
		"exp":     time.Now().Add(time.Minute).Unix(),
	})
	s, err := tok.SignedString(secret)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func resolve(t *testing.T, pool *pgxpool.Pool, instance string) (channel.TenantChannel, error) {
	t.Helper()
	return channel.NewLoader(pool, nil).ByInstance(context.Background(), instance)
}

// TestAPIAuthAndIsolation: a tenant's JWT lists only that tenant's conversations;
// a missing/invalid token is rejected.
func TestAPIAuthAndIsolation(t *testing.T) {
	pool := testutil.Pool(t)
	testutil.SetupControlPlane(t, pool)
	ctx := context.Background()

	a := testutil.ProvisionTenant(t, pool, "inst-a")
	b := testutil.ProvisionTenant(t, pool, "inst-b")

	// Seed one conversation per tenant via the pipeline (fallback reply, no LLM).
	p := pipeline.New(pool, whatsapp.NewRegistry("evolution", &whatsapp.MockSender{}), nil, gating.New(pool))
	chA, _ := resolve(t, pool, "inst-a")
	chB, _ := resolve(t, pool, "inst-b")
	if err := p.Process(ctx, chA, whatsapp.Inbound{Instance: "inst-a", Phone: "111", Text: "oi A"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Process(ctx, chB, whatsapp.Inbound{Instance: "inst-b", Phone: "222", Text: "oi B"}); err != nil {
		t.Fatal(err)
	}

	srv := api.NewServer(pool, authclient.NewVerifier(secret, "sapienza-core"), gating.New(pool), whatsapp.NewRegistry("evolution", &whatsapp.MockSender{}), nil, nil)
	h := srv.Handler()

	// No token → 401.
	rec := do(h, "GET", "/api/v1/conversations", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status %d, want 401", rec.Code)
	}

	// Tenant A token → only A's conversation (phone 111).
	rec = do(h, "GET", "/api/v1/conversations", mint(t, a))
	if rec.Code != http.StatusOK {
		t.Fatalf("A token: status %d, want 200", rec.Code)
	}
	convs := decodeConversations(t, rec)
	if len(convs) != 1 || convs[0].ContactPhone != "111" {
		t.Fatalf("tenant A conversations = %+v, want 1 with phone 111", convs)
	}

	// Tenant B token → only B's conversation (phone 222). Zero leakage.
	rec = do(h, "GET", "/api/v1/conversations", mint(t, b))
	convs = decodeConversations(t, rec)
	if len(convs) != 1 || convs[0].ContactPhone != "222" {
		t.Fatalf("tenant B conversations = %+v, want 1 with phone 222", convs)
	}
}

func TestAPIRejectsInvalidToken(t *testing.T) {
	pool := testutil.Pool(t)
	testutil.SetupControlPlane(t, pool)
	srv := api.NewServer(pool, authclient.NewVerifier(secret, "sapienza-core"), gating.New(pool), whatsapp.NewRegistry("evolution", &whatsapp.MockSender{}), nil, nil)
	rec := do(srv.Handler(), "GET", "/api/v1/conversations", "not-a-jwt")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token: status %d, want 401", rec.Code)
	}
}

// TestChannelOnboarding: vincular o canal (o passo que faltava) cria a linha
// quando não há nenhuma, exige owner/admin, e o getConfig passa a trazer a
// instância. Reproduz o cenário do console "Canal ainda não provisionado".
func TestChannelOnboarding(t *testing.T) {
	pool := testutil.Pool(t)
	testutil.SetupControlPlane(t, pool)
	srv := api.NewServer(pool, authclient.NewVerifier(secret, "sapienza-core"), gating.New(pool), whatsapp.NewRegistry("evolution", &whatsapp.MockSender{}), nil, nil)
	h := srv.Handler()

	// Tenant assinante SEM canal: provisiona e apaga a linha semeada.
	a := testutil.ProvisionTenant(t, pool, "inst-seed")
	if _, err := pool.Exec(context.Background(), `DELETE FROM margot.tenant_channels WHERE tenant_id=$1`, a); err != nil {
		t.Fatal(err)
	}

	if rec := do(h, "GET", "/api/v1/config", mint(t, a)); rec.Code != http.StatusNotFound {
		t.Fatalf("getConfig sem canal: status %d, want 404", rec.Code)
	}
	// member não vincula.
	if rec := doBody(h, "PUT", "/api/v1/channel", mintRole(t, a, "margot", "member"), `{"evolution_instance":"inst-a"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("member: status %d, want 403", rec.Code)
	}
	// instância vazia → 400.
	if rec := doBody(h, "PUT", "/api/v1/channel", mintRole(t, a, "margot", "owner"), `{"evolution_instance":"   "}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("instância vazia: status %d, want 400", rec.Code)
	}
	// owner cria o canal.
	if rec := doBody(h, "PUT", "/api/v1/channel", mintRole(t, a, "margot", "owner"),
		`{"evolution_instance":"inst-a","whatsapp_number":"5521999","dedicated_number_confirmed":true}`); rec.Code != http.StatusOK {
		t.Fatalf("owner cria: status %d (body %s), want 200", rec.Code, rec.Body.String())
	}
	// getConfig agora traz a instância vinculada.
	rec := do(h, "GET", "/api/v1/config", mint(t, a))
	var cfg struct {
		EvolutionInstance string `json:"evolution_instance"`
		WhatsappNumber    string `json:"whatsapp_number"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &cfg)
	if cfg.EvolutionInstance != "inst-a" || cfg.WhatsappNumber != "5521999" {
		t.Fatalf("config = %+v, want instância inst-a e número 5521999", cfg)
	}
}

// TestChannelDuplicateInstance: a instância é UNIQUE — outra tenant não pode
// roubá-la; o erro do banco vira 409, não 500.
func TestChannelDuplicateInstance(t *testing.T) {
	pool := testutil.Pool(t)
	testutil.SetupControlPlane(t, pool)
	srv := api.NewServer(pool, authclient.NewVerifier(secret, "sapienza-core"), gating.New(pool), whatsapp.NewRegistry("evolution", &whatsapp.MockSender{}), nil, nil)

	testutil.ProvisionTenant(t, pool, "inst-taken") // tenant A já usa "inst-taken"
	b := testutil.ProvisionTenant(t, pool, "inst-b")
	rec := doBody(srv.Handler(), "PUT", "/api/v1/channel", mintRole(t, b, "margot", "owner"), `{"evolution_instance":"inst-taken"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("instância duplicada: status %d (body %s), want 409", rec.Code, rec.Body.String())
	}
}

// TestChannelWriteRequiresScope: o write exige token escopado a margot — um token
// sem a claim `produto` (que antes passava) é recusado.
func TestChannelWriteRequiresScope(t *testing.T) {
	pool := testutil.Pool(t)
	testutil.SetupControlPlane(t, pool)
	srv := api.NewServer(pool, authclient.NewVerifier(secret, "sapienza-core"), gating.New(pool), whatsapp.NewRegistry("evolution", &whatsapp.MockSender{}), nil, nil)
	a := testutil.ProvisionTenant(t, pool, "inst-scope")
	rec := doBody(srv.Handler(), "PUT", "/api/v1/channel", mintRole(t, a, "", "owner"), `{"evolution_instance":"x"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("token sem produto: status %d, want 403", rec.Code)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func do(h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func doBody(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// mintRole emite um JWT com produto/role controláveis (produto "" = sem a claim).
func mintRole(t *testing.T, tid uuid.UUID, produto, role string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"uid": uuid.New().String(),
		"tid": tid.String(),
		"iss": "sapienza-core",
		"exp": time.Now().Add(time.Minute).Unix(),
	}
	if produto != "" {
		claims["produto"] = produto
	}
	if role != "" {
		claims["role"] = role
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type convDTO struct {
	ContactPhone string `json:"contact_phone"`
}

func decodeConversations(t *testing.T, rec *httptest.ResponseRecorder) []convDTO {
	t.Helper()
	var out struct {
		Conversations []convDTO `json:"conversations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (body: %s)", err, rec.Body.String())
	}
	return out.Conversations
}
