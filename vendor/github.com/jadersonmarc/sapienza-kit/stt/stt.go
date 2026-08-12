// Package stt is a shared speech-to-text seam for Sapienza products. One contract,
// two products with different needs:
//
//   - Margot Atendente: plain text on short WhatsApp voice notes, low latency.
//   - Margot Editora (Clipes Inteligentes): word-level timestamps on long audio,
//     to drive karaoke captions and cut alignment.
//
// Word timestamps are OPT-IN (Options.WordTimestamps) so the Atendente keeps its
// low-latency path. The default impl is OpenAI-compatible (/audio/transcriptions),
// which works with OpenAI and Groq (Whisper); selected by env.
package stt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"time"
)

// ErrNotConfigured means no STT provider is wired — the caller should fall back.
var ErrNotConfigured = errors.New("stt: provider not configured")

// Word is one token with its time span (milliseconds) in the source audio.
type Word struct {
	Text    string `json:"text"`
	StartMs int    `json:"start_ms"`
	EndMs   int    `json:"end_ms"`
}

// Result carries the transcript text and, when requested, the word alignment.
type Result struct {
	Text  string `json:"text"`
	Lang  string `json:"lang"`
	Words []Word `json:"words"`
}

// Options tunes a transcription. WordTimestamps requests per-word alignment
// (verbose_json) — heavier, so the Atendente leaves it false.
type Options struct {
	WordTimestamps bool
	Language       string // ISO code hint (optional)
}

// Provider turns audio into text (+ optional word timestamps). Configured() reports
// whether a real provider is wired (false => the caller uses a fallback).
type Provider interface {
	Configured() bool
	Transcribe(ctx context.Context, audio []byte, mime string, opts Options) (Result, error)
}

// Noop is the disabled seam: no provider configured.
type Noop struct{}

func (Noop) Configured() bool { return false }
func (Noop) Transcribe(context.Context, []byte, string, Options) (Result, error) {
	return Result{}, ErrNotConfigured
}

// HTTP calls an OpenAI-compatible /audio/transcriptions endpoint (OpenAI or Groq).
type HTTP struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

// NewHTTP builds an HTTP provider. timeout defaults to 5min (long audio) if zero.
func NewHTTP(baseURL, apiKey, model string, timeout time.Duration) *HTTP {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &HTTP{baseURL: baseURL, apiKey: apiKey, model: model, http: &http.Client{Timeout: timeout}}
}

// FromEnv returns the env-configured provider: an HTTP provider when STT_API_KEY is
// set, otherwise Noop. STT_BASE_URL defaults to OpenAI; point it at Groq
// (https://api.groq.com/openai/v1) + STT_MODEL=whisper-large-v3-turbo to use Groq.
func FromEnv() Provider {
	key := os.Getenv("STT_API_KEY")
	if key == "" {
		return Noop{}
	}
	base := os.Getenv("STT_BASE_URL")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	model := os.Getenv("STT_MODEL")
	if model == "" {
		model = "whisper-1"
	}
	return NewHTTP(base, key, model, 0)
}

func (t *HTTP) Configured() bool { return true }

func (t *HTTP) Transcribe(ctx context.Context, audio []byte, mime string, opts Options) (Result, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filenameFor(mime))
	if err != nil {
		return Result{}, err
	}
	if _, err := fw.Write(audio); err != nil {
		return Result{}, err
	}
	if err := mw.WriteField("model", t.model); err != nil {
		return Result{}, err
	}
	if opts.Language != "" {
		if err := mw.WriteField("language", opts.Language); err != nil {
			return Result{}, err
		}
	}
	// Word timestamps: verbose_json + per-word granularity (OpenAI/Groq Whisper).
	if opts.WordTimestamps {
		if err := mw.WriteField("response_format", "verbose_json"); err != nil {
			return Result{}, err
		}
		if err := mw.WriteField("timestamp_granularities[]", "word"); err != nil {
			return Result{}, err
		}
	}
	if err := mw.Close(); err != nil {
		return Result{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+"/audio/transcriptions", &buf)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Authorization", "Bearer "+t.apiKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := t.http.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= http.StatusMultipleChoices {
		return Result{}, fmt.Errorf("stt: status %d: %s", resp.StatusCode, string(body))
	}

	// Both plain and verbose_json carry "text"; verbose_json adds language + words.
	var out struct {
		Text     string `json:"text"`
		Language string `json:"language"`
		Words    []struct {
			Word  string  `json:"word"`
			Start float64 `json:"start"` // seconds
			End   float64 `json:"end"`
		} `json:"words"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return Result{}, fmt.Errorf("stt: decode: %w", err)
	}
	res := Result{Text: out.Text, Lang: out.Language}
	for _, w := range out.Words {
		res.Words = append(res.Words, Word{
			Text:    w.Word,
			StartMs: int(math.Round(w.Start * 1000)),
			EndMs:   int(math.Round(w.End * 1000)),
		})
	}
	return res, nil
}

// filenameFor picks an extension by mime so the provider detects the format.
func filenameFor(mime string) string {
	switch {
	case bytes.Contains([]byte(mime), []byte("ogg")):
		return "audio.ogg"
	case bytes.Contains([]byte(mime), []byte("mp4")), bytes.Contains([]byte(mime), []byte("m4a")):
		return "audio.m4a"
	case bytes.Contains([]byte(mime), []byte("wav")):
		return "audio.wav"
	case bytes.Contains([]byte(mime), []byte("mpeg")), bytes.Contains([]byte(mime), []byte("mp3")):
		return "audio.mp3"
	default:
		return "audio.ogg" // WhatsApp voice notes são ogg/opus por padrão
	}
}

// Mock returns a fixed result — used in tests.
type Mock struct {
	Result Result
	Err    error
}

func (m Mock) Configured() bool { return true }
func (m Mock) Transcribe(context.Context, []byte, string, Options) (Result, error) {
	return m.Result, m.Err
}
