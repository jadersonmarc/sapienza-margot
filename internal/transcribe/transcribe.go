// Package transcribe converts audio (WhatsApp voice notes) to text. It is a seam:
// the pipeline depends only on the Transcriber interface. The default HTTP impl is
// OpenAI-compatible (works with OpenAI and Groq — same /audio/transcriptions shape),
// selected by env. Without STT_API_KEY it is disabled (Noop) and the pipeline sends
// a polite text fallback instead of transcribing.
package transcribe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"time"
)

// ErrNotConfigured means no STT provider is set — the caller should fall back.
var ErrNotConfigured = errors.New("transcribe: provider not configured")

// Transcriber turns audio bytes into text. Configured() reports whether a real
// provider is wired (false => the pipeline uses the text fallback).
type Transcriber interface {
	Configured() bool
	Transcribe(ctx context.Context, audio []byte, mime string) (string, error)
}

// New returns the env-configured Transcriber: an HTTPTranscriber when STT_API_KEY
// is set, otherwise a NoopTranscriber. STT_BASE_URL defaults to OpenAI; point it at
// Groq (https://api.groq.com/openai/v1) + STT_MODEL=whisper-large-v3 to use Groq.
func New() Transcriber {
	key := os.Getenv("STT_API_KEY")
	if key == "" {
		return NoopTranscriber{}
	}
	base := os.Getenv("STT_BASE_URL")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	model := os.Getenv("STT_MODEL")
	if model == "" {
		model = "whisper-1"
	}
	return &HTTPTranscriber{
		baseURL: base,
		apiKey:  key,
		model:   model,
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

// NoopTranscriber is the disabled seam: no provider configured.
type NoopTranscriber struct{}

func (NoopTranscriber) Configured() bool { return false }
func (NoopTranscriber) Transcribe(context.Context, []byte, string) (string, error) {
	return "", ErrNotConfigured
}

// HTTPTranscriber calls an OpenAI-compatible /audio/transcriptions endpoint
// (OpenAI or Groq) with a multipart upload.
type HTTPTranscriber struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

func (t *HTTPTranscriber) Configured() bool { return true }

func (t *HTTPTranscriber) Transcribe(ctx context.Context, audio []byte, mime string) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filenameFor(mime))
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(audio); err != nil {
		return "", err
	}
	if err := mw.WriteField("model", t.model); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	url := t.baseURL + "/audio/transcriptions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+t.apiKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := t.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("transcribe: status %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("transcribe: decode: %w", err)
	}
	return out.Text, nil
}

// filenameFor picks an extension by mime so the provider detects the format.
func filenameFor(mime string) string {
	switch {
	case bytes.Contains([]byte(mime), []byte("ogg")):
		return "audio.ogg"
	case bytes.Contains([]byte(mime), []byte("mp4")), bytes.Contains([]byte(mime), []byte("m4a")):
		return "audio.m4a"
	case bytes.Contains([]byte(mime), []byte("mpeg")), bytes.Contains([]byte(mime), []byte("mp3")):
		return "audio.mp3"
	default:
		return "audio.ogg" // WhatsApp voice notes são ogg/opus por padrão
	}
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
