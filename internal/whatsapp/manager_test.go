package whatsapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jadersonmarc/sapienza-margot/internal/whatsapp"
)

// fakeEvolution finge o Evolution v2: guarda o que recebeu no create para o teste
// asserir o webhook+secret embutidos.
type createBody struct {
	InstanceName string `json:"instanceName"`
	Integration  string `json:"integration"`
	QRCode       bool   `json:"qrcode"`
	Webhook      struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Events  []string          `json:"events"`
	} `json:"webhook"`
}

func TestManagerCreateConnectState(t *testing.T) {
	var got createBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("apikey") != "global-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/instance/create":
			_ = json.NewDecoder(r.Body).Decode(&got)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"hash":{"apikey":"inst-key"},"qrcode":{"base64":"data:image/png;base64,AAAA"}}`))
		case r.Method == "GET" && r.URL.Path == "/instance/connect/margot-x":
			_, _ = w.Write([]byte(`{"base64":"data:image/png;base64,BBBB"}`))
		case r.Method == "GET" && r.URL.Path == "/instance/connectionState/margot-x":
			_, _ = w.Write([]byte(`{"instance":{"state":"open","ownerJid":"5521999@s.whatsapp.net"}}`))
		default:
			http.Error(w, "not found: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	m := whatsapp.NewManager(srv.URL, "global-key")
	if !m.Configured() {
		t.Fatal("Configured() = false, want true")
	}
	ctx := context.Background()

	// create: o webhook e o segredo vão embutidos, evento MESSAGES_UPSERT.
	if err := m.CreateInstance(ctx, "margot-x", "https://margot.example/webhook/evolution", "s3cr3t"); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if got.InstanceName != "margot-x" || got.Integration != "WHATSAPP-BAILEYS" || !got.QRCode {
		t.Fatalf("create body inesperado: %+v", got)
	}
	if got.Webhook.URL != "https://margot.example/webhook/evolution" {
		t.Fatalf("webhook url = %q", got.Webhook.URL)
	}
	if got.Webhook.Headers["apikey"] != "s3cr3t" {
		t.Fatalf("webhook secret = %q, want s3cr3t", got.Webhook.Headers["apikey"])
	}
	if len(got.Webhook.Events) != 1 || got.Webhook.Events[0] != "MESSAGES_UPSERT" {
		t.Fatalf("webhook events = %v", got.Webhook.Events)
	}

	// connect: devolve o QR.
	qr, err := m.ConnectQR(ctx, "margot-x")
	if err != nil {
		t.Fatalf("ConnectQR: %v", err)
	}
	if qr != "data:image/png;base64,BBBB" {
		t.Fatalf("qr = %q", qr)
	}

	// state: conectado + número extraído do ownerJid.
	state, number, err := m.State(ctx, "margot-x")
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state != "open" || number != "5521999" {
		t.Fatalf("state=%q number=%q, want open/5521999", state, number)
	}
}

// Instância já existente no create → tratado como sucesso (o caller re-busca o QR).
func TestManagerCreateAlreadyExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"This name is already in use"}`))
	}))
	defer srv.Close()
	m := whatsapp.NewManager(srv.URL, "k")
	if err := m.CreateInstance(context.Background(), "margot-x", "https://x/webhook", "s"); err != nil {
		t.Fatalf("instância existente deveria ser ok, got %v", err)
	}
}

func TestManagerNotConfigured(t *testing.T) {
	if whatsapp.NewManager("", "").Configured() {
		t.Fatal("sem URL/key deveria ser não-configurado")
	}
}
