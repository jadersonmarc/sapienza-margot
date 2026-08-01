package transcribe_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jadersonmarc/sapienza-margot/internal/transcribe"
)

func TestNoopDesligado(t *testing.T) {
	// Sem STT_API_KEY, New() devolve o Noop (desligado).
	t.Setenv("STT_API_KEY", "")
	tr := transcribe.New()
	if tr.Configured() {
		t.Fatal("sem chave deveria estar desligado")
	}
	if _, err := tr.Transcribe(context.Background(), []byte("x"), "audio/ogg"); err == nil {
		t.Fatal("Transcribe sem provider deveria falhar")
	}
}

func TestHTTPTranscriberMultipart(t *testing.T) {
	var gotModel, gotAuth string
	var gotFile bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = r.ParseMultipartForm(1 << 20)
		gotModel = r.FormValue("model")
		if r.MultipartForm != nil {
			_, ok := r.MultipartForm.File["file"]
			gotFile = ok
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"olá quero o preço"}`))
	}))
	defer srv.Close()

	t.Setenv("STT_API_KEY", "sk-test")
	t.Setenv("STT_BASE_URL", srv.URL)
	t.Setenv("STT_MODEL", "whisper-large-v3")
	tr := transcribe.New()
	if !tr.Configured() {
		t.Fatal("com chave deveria estar configurado")
	}
	text, err := tr.Transcribe(context.Background(), []byte("OGGBYTES"), "audio/ogg; codecs=opus")
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	if text != "olá quero o preço" {
		t.Fatalf("texto = %q", text)
	}
	if gotModel != "whisper-large-v3" {
		t.Fatalf("model enviado = %q", gotModel)
	}
	if !strings.HasPrefix(gotAuth, "Bearer sk-test") {
		t.Fatalf("auth = %q", gotAuth)
	}
	if !gotFile {
		t.Fatal("o campo file não foi enviado no multipart")
	}
}

func TestHTTPTranscriberPropagaErro(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()

	t.Setenv("STT_API_KEY", "sk-bad")
	t.Setenv("STT_BASE_URL", srv.URL)
	tr := transcribe.New()
	if _, err := tr.Transcribe(context.Background(), []byte("x"), "audio/ogg"); err == nil {
		t.Fatal("status 401 deveria virar erro")
	}
}
