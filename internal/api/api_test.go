package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

	srv := api.NewServer(pool, authclient.NewVerifier(secret, "sapienza-core"), gating.New(pool), whatsapp.NewRegistry("evolution", &whatsapp.MockSender{}), nil)
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
	srv := api.NewServer(pool, authclient.NewVerifier(secret, "sapienza-core"), gating.New(pool), whatsapp.NewRegistry("evolution", &whatsapp.MockSender{}), nil)
	rec := do(srv.Handler(), "GET", "/api/v1/conversations", "not-a-jwt")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token: status %d, want 401", rec.Code)
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
