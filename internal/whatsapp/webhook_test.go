package whatsapp_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jadersonmarc/sapienza-margot/internal/channel"
	"github.com/jadersonmarc/sapienza-margot/internal/whatsapp"
)

type fakeResolver struct {
	tenantID uuid.UUID
	secret   string // segredo do tenant; vazio => cai no global
	err      error  // instância desconhecida
}

func (f fakeResolver) ByInstance(_ context.Context, instance string) (channel.TenantChannel, error) {
	if f.err != nil {
		return channel.TenantChannel{}, f.err
	}
	return channel.TenantChannel{TenantID: f.tenantID, EvolutionInstance: instance, WebhookSecret: f.secret}, nil
}

type fakeProcessor struct{ calls int }

func (f *fakeProcessor) Process(_ context.Context, _ channel.TenantChannel, _ whatsapp.Inbound) error {
	f.calls++
	return nil
}

const body = `{"event":"messages.upsert","instance":"inst-a","data":{"key":{"remoteJid":"5511999@s.whatsapp.net","fromMe":false,"id":"m1"},"pushName":"Ana","message":{"conversation":"oi"}}}`

func newHandler(proc *fakeProcessor) *whatsapp.Handler {
	return whatsapp.NewHandler(fakeResolver{tenantID: uuid.New()}, proc, "s3cr3t")
}

func TestWebhookRejectsBadApiKey(t *testing.T) {
	proc := &fakeProcessor{}
	req := httptest.NewRequest(http.MethodPost, "/webhook/evolution", strings.NewReader(body))
	req.Header.Set("apikey", "wrong")
	rec := httptest.NewRecorder()
	newHandler(proc).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if proc.calls != 0 {
		t.Fatal("processor should not run on bad apikey")
	}
}

func TestWebhookProcessesWithGoodApiKey(t *testing.T) {
	proc := &fakeProcessor{}
	req := httptest.NewRequest(http.MethodPost, "/webhook/evolution", strings.NewReader(body))
	req.Header.Set("apikey", "s3cr3t")
	rec := httptest.NewRecorder()
	newHandler(proc).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if proc.calls != 1 {
		t.Fatalf("processor calls = %d, want 1", proc.calls)
	}
}

func TestWebhookIgnoresNonMessageEvents(t *testing.T) {
	proc := &fakeProcessor{}
	req := httptest.NewRequest(http.MethodPost, "/webhook/evolution",
		strings.NewReader(`{"event":"connection.update","instance":"inst-a","data":{}}`))
	req.Header.Set("apikey", "s3cr3t")
	rec := httptest.NewRecorder()
	newHandler(proc).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || proc.calls != 0 {
		t.Fatalf("non-message event: status=%d calls=%d, want 200/0", rec.Code, proc.calls)
	}
}

func TestWebhookRejectsGet(t *testing.T) {
	rec := httptest.NewRecorder()
	newHandler(&fakeProcessor{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/webhook/evolution", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// post envia o webhook com um apikey, contra um handler com segredo global e um
// resolver que devolve o canal (com ou sem segredo próprio).
func post(t *testing.T, r fakeResolver, globalSecret, apikey string) (int, int) {
	t.Helper()
	proc := &fakeProcessor{}
	req := httptest.NewRequest(http.MethodPost, "/webhook/evolution", strings.NewReader(body))
	if apikey != "" {
		req.Header.Set("apikey", apikey)
	}
	rec := httptest.NewRecorder()
	whatsapp.NewHandler(r, proc, globalSecret).ServeHTTP(rec, req)
	return rec.Code, proc.calls
}

// Com um segredo único para todas as instâncias, quem o tivesse poderia forjar um
// payload dizendo ser a instância de qualquer cliente — injetando mensagem no
// schema dele e gastando nosso orçamento de modelo. O segredo passa a ser do tenant.
func TestWebhookSegredoPorTenant(t *testing.T) {
	tenant := fakeResolver{tenantID: uuid.New(), secret: "do-tenant"}

	// O segredo do tenant autoriza.
	if code, calls := post(t, tenant, "global", "do-tenant"); code != http.StatusOK || calls != 1 {
		t.Fatalf("segredo do tenant: status=%d calls=%d, want 200/1", code, calls)
	}
	// O global NÃO serve para quem já tem o seu — é o ponto do isolamento.
	if code, calls := post(t, tenant, "global", "global"); code != http.StatusUnauthorized || calls != 0 {
		t.Fatalf("global não pode abrir tenant com segredo próprio: status=%d calls=%d, want 401/0", code, calls)
	}
	// Segredo de outro tenant também não.
	if code, _ := post(t, tenant, "global", "de-outro"); code != http.StatusUnauthorized {
		t.Fatalf("segredo alheio: status=%d, want 401", code)
	}
}

// Enquanto o tenant não roda a rotação, o segredo global continua valendo — a
// migração para segredo por tenant não derruba quem já está configurado.
func TestWebhookFallbackNoSegredoGlobal(t *testing.T) {
	semSegredo := fakeResolver{tenantID: uuid.New()} // ainda não rotacionou
	if code, calls := post(t, semSegredo, "global", "global"); code != http.StatusOK || calls != 1 {
		t.Fatalf("fallback global: status=%d calls=%d, want 200/1", code, calls)
	}
	if code, _ := post(t, semSegredo, "global", "errado"); code != http.StatusUnauthorized {
		t.Fatalf("apikey errada no fallback: status=%d, want 401", code)
	}
}

// Antes, secret vazio devolvia true e liberava o endpoint inteiro ("dev only") —
// uma env ausente em produção abria a porta em silêncio.
func TestWebhookSemSegredoNenhumRecusa(t *testing.T) {
	semSegredo := fakeResolver{tenantID: uuid.New()}
	code, calls := post(t, semSegredo, "", "qualquer-coisa")
	if code != http.StatusUnauthorized {
		t.Fatalf("sem segredo configurado deve recusar (fail-closed): status=%d, want 401", code)
	}
	if calls != 0 {
		t.Fatal("nada pode ser processado sem segredo configurado")
	}
	// Nem sem apikey nenhuma.
	if code, _ := post(t, semSegredo, "", ""); code != http.StatusUnauthorized {
		t.Fatalf("sem segredo e sem apikey: status=%d, want 401", code)
	}
}

// Instância desconhecida continua 200: 401 aqui viraria um oráculo para descobrir
// quais instâncias existem, e o 200 é o que faz a Evolution parar de retentar.
func TestWebhookInstanciaDesconhecidaNaoVazaExistencia(t *testing.T) {
	desconhecida := fakeResolver{err: errNoChannel}
	code, calls := post(t, desconhecida, "global", "global")
	if code != http.StatusOK || calls != 0 {
		t.Fatalf("instância desconhecida: status=%d calls=%d, want 200/0", code, calls)
	}
}

var errNoChannel = errors.New("no channel for instance")
