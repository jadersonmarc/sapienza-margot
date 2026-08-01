package whatsapp

import (
	"encoding/json"
	"testing"
)

func parse(t *testing.T, raw string) (Inbound, bool) {
	t.Helper()
	var w evolutionWebhook
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return parseInbound(w)
}

func TestParseInboundAudio(t *testing.T) {
	in, ok := parse(t, `{"instance":"i","data":{"key":{"remoteJid":"5511988887777@s.whatsapp.net","id":"m1"},"message":{"audioMessage":{"mimetype":"audio/ogg; codecs=opus"}}}}`)
	if !ok {
		t.Fatal("deveria ser acionável")
	}
	if !in.IsAudio {
		t.Fatal("IsAudio deveria ser true para audioMessage")
	}
	if in.Text != "" {
		t.Fatalf("Text = %q, want vazio", in.Text)
	}
	if in.ProviderID != "m1" {
		t.Fatalf("ProviderID = %q", in.ProviderID)
	}
}

func TestParseInboundTextoNaoEhAudio(t *testing.T) {
	in, ok := parse(t, `{"instance":"i","data":{"key":{"remoteJid":"5511988887777@s.whatsapp.net","id":"m2"},"message":{"conversation":"oi"}}}`)
	if !ok || in.IsAudio || in.Text != "oi" {
		t.Fatalf("texto simples: ok=%v IsAudio=%v Text=%q", ok, in.IsAudio, in.Text)
	}
}
