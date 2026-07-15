package whatsapp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jadersonmarc/sapienza-margot/internal/channel"
	"github.com/jadersonmarc/sapienza-margot/internal/whatsapp"
)

type fakeResolver struct{ tenantID uuid.UUID }

func (f fakeResolver) ByInstance(_ context.Context, instance string) (channel.TenantChannel, error) {
	return channel.TenantChannel{TenantID: f.tenantID, EvolutionInstance: instance}, nil
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
