package api_test

import (
	"bytes"
	"context"
	"encoding/base64"
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
	"github.com/jadersonmarc/sapienza-margot/internal/agent"
	"github.com/jadersonmarc/sapienza-margot/internal/pipeline"
	"github.com/jadersonmarc/sapienza-margot/internal/secrets"
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

	srv := api.NewServer(pool, authclient.NewVerifier(secret, "sapienza-core"), gating.New(pool), whatsapp.NewRegistry("evolution", &whatsapp.MockSender{}), nil, nil, nil, "")
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
	srv := api.NewServer(pool, authclient.NewVerifier(secret, "sapienza-core"), gating.New(pool), whatsapp.NewRegistry("evolution", &whatsapp.MockSender{}), nil, nil, nil, "")
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
	srv := api.NewServer(pool, authclient.NewVerifier(secret, "sapienza-core"), gating.New(pool), whatsapp.NewRegistry("evolution", &whatsapp.MockSender{}), nil, nil, nil, "")
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
	srv := api.NewServer(pool, authclient.NewVerifier(secret, "sapienza-core"), gating.New(pool), whatsapp.NewRegistry("evolution", &whatsapp.MockSender{}), nil, nil, nil, "")

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
	srv := api.NewServer(pool, authclient.NewVerifier(secret, "sapienza-core"), gating.New(pool), whatsapp.NewRegistry("evolution", &whatsapp.MockSender{}), nil, nil, nil, "")
	a := testutil.ProvisionTenant(t, pool, "inst-scope")
	rec := doBody(srv.Handler(), "PUT", "/api/v1/channel", mintRole(t, a, "", "owner"), `{"evolution_instance":"x"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("token sem produto: status %d, want 403", rec.Code)
	}
}

// fakeProvisioner finge o Evolution: registra o create e devolve QR/estado
// controláveis, sem servidor real.
type fakeProvisioner struct {
	created   map[string]string // instance → webhook secret
	qr        string
	state     string
	number    string
	notConfig bool
}

func (f *fakeProvisioner) Configured() bool { return !f.notConfig }
func (f *fakeProvisioner) CreateInstance(_ context.Context, name, _, secret string) error {
	if f.created == nil {
		f.created = map[string]string{}
	}
	f.created[name] = secret
	return nil
}
func (f *fakeProvisioner) ConnectQR(_ context.Context, _ string) (string, error) { return f.qr, nil }
func (f *fakeProvisioner) State(_ context.Context, _ string) (string, string, error) {
	return f.state, f.number, nil
}

func newServerP(t *testing.T, pool *pgxpool.Pool, prov api.ChannelProvisioner) *api.Server {
	t.Helper()
	// cipher real (o connect cifra o segredo do webhook). Chave fixa de 32 bytes.
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	ciph, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return api.NewServer(pool, authclient.NewVerifier(secret, "sapienza-core"), gating.New(pool),
		whatsapp.NewRegistry("evolution", &whatsapp.MockSender{}), ciph, nil, prov, "https://margot.test/webhook/evolution")
}

// TestConnectChannel: o fluxo self-serve cria a instância no Evolution (com o
// webhook+secret), grava a linha e devolve o QR; exige owner/admin.
func TestConnectChannel(t *testing.T) {
	pool := testutil.Pool(t)
	testutil.SetupControlPlane(t, pool)
	a := testutil.ProvisionTenant(t, pool, "seed-x")
	if _, err := pool.Exec(context.Background(), `DELETE FROM margot.tenant_channels WHERE tenant_id=$1`, a); err != nil {
		t.Fatal(err)
	}
	prov := &fakeProvisioner{qr: "data:image/png;base64,QQQ", state: "connecting"}
	h := newServerP(t, pool, prov).Handler()

	// member não conecta.
	if rec := doBody(h, "POST", "/api/v1/channel/connect", mintRole(t, a, "margot", "member"), ""); rec.Code != http.StatusForbidden {
		t.Fatalf("member connect: status %d, want 403", rec.Code)
	}
	// owner conecta → 200 com o QR.
	rec := doBody(h, "POST", "/api/v1/channel/connect", mintRole(t, a, "margot", "owner"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("owner connect: status %d (body %s), want 200", rec.Code, rec.Body.String())
	}
	var out struct {
		QR string `json:"qr_base64"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.QR != "data:image/png;base64,QQQ" {
		t.Fatalf("qr = %q", out.QR)
	}
	// a instância criada segue o padrão margot-<tenant sem hífen> e recebeu um segredo.
	name := "margot-" + strings.ReplaceAll(a.String(), "-", "")
	if prov.created[name] == "" {
		t.Fatalf("instância %q não foi criada com segredo; created=%v", name, prov.created)
	}
	// a linha nasceu com a instância e o driver evolution.
	var inst, driver string
	_ = pool.QueryRow(context.Background(),
		`SELECT evolution_instance, driver FROM margot.tenant_channels WHERE tenant_id=$1`, a).Scan(&inst, &driver)
	if inst != name || driver != "evolution" {
		t.Fatalf("linha = (%q,%q), want (%q,evolution)", inst, driver, name)
	}
}

// TestChannelStatusOpen: quando o Evolution reporta open, status devolve conectado
// e grava o número + confirma o dedicado.
func TestChannelStatusOpen(t *testing.T) {
	pool := testutil.Pool(t)
	testutil.SetupControlPlane(t, pool)
	a := testutil.ProvisionTenant(t, pool, "st-x") // já cria uma linha (seed)
	prov := &fakeProvisioner{state: "open", number: "5521988887777"}
	h := newServerP(t, pool, prov).Handler()

	rec := do(h, "GET", "/api/v1/channel/status", mintRole(t, a, "margot", "owner"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d (body %s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Connected bool   `json:"connected"`
		State     string `json:"state"`
		Number    string `json:"number"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.Connected || out.State != "open" || out.Number != "5521988887777" {
		t.Fatalf("status out = %+v", out)
	}
	var dedicated bool
	_ = pool.QueryRow(context.Background(),
		`SELECT dedicated_number_confirmed FROM margot.tenant_channels WHERE tenant_id=$1`, a).Scan(&dedicated)
	if !dedicated {
		t.Fatal("conectar deveria marcar dedicated_number_confirmed")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

type contactDTO struct {
	ID      string  `json:"id"`
	Phone   string  `json:"phone"`
	Name    *string `json:"name"`
	Consent bool    `json:"consent"`
}

func decodeContacts(t *testing.T, rec *httptest.ResponseRecorder) []contactDTO {
	t.Helper()
	var body struct {
		Contacts []contactDTO `json:"contacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode contacts: %v (%s)", err, rec.Body.String())
	}
	return body.Contacts
}

func TestContactsCRM(t *testing.T) {
	pool := testutil.Pool(t)
	testutil.SetupControlPlane(t, pool)
	ctx := context.Background()

	tn := testutil.ProvisionTenant(t, pool, "inst-crm")
	// Um inbound cria o contato (via pipeline, fallback sem LLM).
	p := pipeline.New(pool, whatsapp.NewRegistry("evolution", &whatsapp.MockSender{}), nil, gating.New(pool))
	ch, _ := resolve(t, pool, "inst-crm")
	if err := p.Process(ctx, ch, whatsapp.Inbound{Instance: "inst-crm", Phone: "5599", Text: "oi", PushName: "Zé"}); err != nil {
		t.Fatal(err)
	}

	srv := api.NewServer(pool, authclient.NewVerifier(secret, "sapienza-core"), gating.New(pool), whatsapp.NewRegistry("evolution", &whatsapp.MockSender{}), nil, nil, nil, "")
	h := srv.Handler()
	owner := mintRole(t, tn, "margot", "owner")

	// Lista contatos → 1.
	got := decodeContacts(t, do(h, "GET", "/api/v1/contacts", owner))
	if len(got) != 1 || got[0].Phone != "5599" {
		t.Fatalf("contacts = %+v, want 1 com phone 5599", got)
	}
	id := got[0].ID

	// Pipeline → 200.
	if rec := do(h, "GET", "/api/v1/pipeline", owner); rec.Code != http.StatusOK {
		t.Fatalf("pipeline status %d", rec.Code)
	}

	// PATCH nome + consent → 200 e persiste.
	if rec := doBody(h, "PATCH", "/api/v1/contacts/"+id, owner, `{"name":"Novo Nome","consent":true}`); rec.Code != http.StatusOK {
		t.Fatalf("patch status %d: %s", rec.Code, rec.Body.String())
	}
	got = decodeContacts(t, do(h, "GET", "/api/v1/contacts", owner))
	if got[0].Name == nil || *got[0].Name != "Novo Nome" || !got[0].Consent {
		t.Fatalf("após patch = %+v", got[0])
	}

	// DELETE → 200, some (cascata LGPD).
	if rec := doBody(h, "DELETE", "/api/v1/contacts/"+id, owner, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete status %d", rec.Code)
	}
	if got = decodeContacts(t, do(h, "GET", "/api/v1/contacts", owner)); len(got) != 0 {
		t.Fatalf("após delete = %+v, want vazio", got)
	}
}

type autoDTO struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Enabled bool   `json:"enabled"`
}

func decodeAutomations(t *testing.T, rec *httptest.ResponseRecorder) []autoDTO {
	t.Helper()
	var body struct {
		Automations []autoDTO `json:"automations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode automations: %v (%s)", err, rec.Body.String())
	}
	return body.Automations
}

func TestAutomationsCRUD(t *testing.T) {
	pool := testutil.Pool(t)
	testutil.SetupControlPlane(t, pool)

	tn := testutil.ProvisionTenant(t, pool, "inst-auto")
	srv := api.NewServer(pool, authclient.NewVerifier(secret, "sapienza-core"), gating.New(pool), whatsapp.NewRegistry("evolution", &whatsapp.MockSender{}), nil, nil, nil, "")
	h := srv.Handler()
	owner := mintRole(t, tn, "margot", "owner")

	// Cria uma regra de keyword.
	create := `{"type":"keyword","trigger":{"keywords":["preço"]},"action":{"reply":"Segue a tabela"},"enabled":true,"position":1}`
	rec := doBody(h, "POST", "/api/v1/automations", owner, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// Lista → 1.
	got := decodeAutomations(t, do(h, "GET", "/api/v1/automations", owner))
	if len(got) != 1 || got[0].Type != "keyword" || !got[0].Enabled {
		t.Fatalf("automations = %+v", got)
	}

	// Atualiza (desabilita).
	upd := `{"type":"keyword","trigger":{"keywords":["preço","valor"]},"action":{"reply":"x"},"enabled":false,"position":1}`
	if rec := doBody(h, "PUT", "/api/v1/automations/"+created.ID, owner, upd); rec.Code != http.StatusOK {
		t.Fatalf("update status %d: %s", rec.Code, rec.Body.String())
	}
	got = decodeAutomations(t, do(h, "GET", "/api/v1/automations", owner))
	if got[0].Enabled {
		t.Fatalf("após update deveria estar desabilitada: %+v", got[0])
	}

	// type inválido → 400.
	if rec := doBody(h, "POST", "/api/v1/automations", owner, `{"type":"xpto"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("type inválido status %d", rec.Code)
	}

	// member (não-manager) não cria → 403.
	member := mintRole(t, tn, "margot", "member")
	if rec := doBody(h, "POST", "/api/v1/automations", member, create); rec.Code != http.StatusForbidden {
		t.Fatalf("member create status %d, want 403", rec.Code)
	}

	// Exclui → vazio.
	if rec := doBody(h, "DELETE", "/api/v1/automations/"+created.ID, owner, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete status %d", rec.Code)
	}
	if got = decodeAutomations(t, do(h, "GET", "/api/v1/automations", owner)); len(got) != 0 {
		t.Fatalf("após delete = %+v, want vazio", got)
	}
}

type fakeReplier struct{}

func (fakeReplier) Reply(_ context.Context, _, _ string, _ []agent.Turn, _ int) (string, error) {
	return "Sugestão: nosso preço começa em X.", nil
}

func TestSuggest(t *testing.T) {
	pool := testutil.Pool(t)
	testutil.SetupControlPlane(t, pool)
	ctx := context.Background()

	tn := testutil.ProvisionTenant(t, pool, "inst-sug")
	p := pipeline.New(pool, whatsapp.NewRegistry("evolution", &whatsapp.MockSender{}), nil, gating.New(pool))
	ch, _ := resolve(t, pool, "inst-sug")
	if err := p.Process(ctx, ch, whatsapp.Inbound{Instance: "inst-sug", Phone: "5511", Text: "quero saber o preço"}); err != nil {
		t.Fatal(err)
	}

	srv := api.NewServer(pool, authclient.NewVerifier(secret, "sapienza-core"), gating.New(pool), whatsapp.NewRegistry("evolution", &whatsapp.MockSender{}), nil, nil, nil, "")
	srv.SetReplier(fakeReplier{})
	h := srv.Handler()
	owner := mintRole(t, tn, "margot", "owner")

	convs := decodeConversations(t, do(h, "GET", "/api/v1/conversations", owner))
	if len(convs) != 1 || convs[0].ID == "" {
		t.Fatalf("convs = %+v", convs)
	}
	id := convs[0].ID

	rec := doBody(h, "POST", "/api/v1/conversations/"+id+"/suggest", owner, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("suggest status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Suggestion string `json:"suggestion"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Suggestion == "" {
		t.Fatalf("sugestão vazia")
	}

	// Sem Replier ligado → 503.
	srv2 := api.NewServer(pool, authclient.NewVerifier(secret, "sapienza-core"), gating.New(pool), whatsapp.NewRegistry("evolution", &whatsapp.MockSender{}), nil, nil, nil, "")
	if rec := doBody(srv2.Handler(), "POST", "/api/v1/conversations/"+id+"/suggest", owner, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("sem replier status %d, want 503", rec.Code)
	}
}

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
	ID           string `json:"id"`
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
