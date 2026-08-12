// Package transcribe converts audio (WhatsApp voice notes) to text. It is a seam:
// the pipeline depends only on the Transcriber interface. Under the hood it now
// delegates to the shared kit/stt provider (one STT contract for the Atendente and
// the Editora/Clipper). The Atendente needs only plain text at low latency, so it
// asks for no word timestamps. Without STT_API_KEY the provider is Noop (disabled)
// and the pipeline sends a polite text fallback instead of transcribing.
package transcribe

import (
	"context"

	"github.com/jadersonmarc/sapienza-kit/stt"
)

// ErrNotConfigured means no STT provider is set — the caller should fall back.
var ErrNotConfigured = stt.ErrNotConfigured

// Transcriber turns audio bytes into text. Configured() reports whether a real
// provider is wired (false => the pipeline uses the text fallback).
type Transcriber interface {
	Configured() bool
	Transcribe(ctx context.Context, audio []byte, mime string) (string, error)
}

// New returns the env-configured Transcriber backed by the shared kit/stt provider
// (HTTP/Whisper via STT_* envs; Noop when STT_API_KEY is unset). Point STT_BASE_URL
// at Groq (https://api.groq.com/openai/v1) + STT_MODEL=whisper-large-v3-turbo to use Groq.
func New() Transcriber {
	return kitTranscriber{p: stt.FromEnv()}
}

// kitTranscriber adapts the shared kit/stt.Provider to the Atendente's text-only
// seam (no word timestamps — short audio, low latency).
type kitTranscriber struct{ p stt.Provider }

func (k kitTranscriber) Configured() bool { return k.p.Configured() }

func (k kitTranscriber) Transcribe(ctx context.Context, audio []byte, mime string) (string, error) {
	res, err := k.p.Transcribe(ctx, audio, mime, stt.Options{})
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

// NoopTranscriber is the disabled seam: no provider configured.
type NoopTranscriber struct{}

func (NoopTranscriber) Configured() bool { return false }
func (NoopTranscriber) Transcribe(context.Context, []byte, string) (string, error) {
	return "", ErrNotConfigured
}

// MockTranscriber returns a fixed transcript — used in tests.
type MockTranscriber struct {
	Text string
	Err  error
}

func (m MockTranscriber) Configured() bool { return true }
func (m MockTranscriber) Transcribe(context.Context, []byte, string) (string, error) {
	return m.Text, m.Err
}
