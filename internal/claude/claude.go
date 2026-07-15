// Package claude implements the agent.Replier seam using the Anthropic API.
// Auto-reply uses Haiku by default; the suggestion endpoint (Fase 4) reuses the
// same Replier with a Sonnet model id.
package claude

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/jadersonmarc/sapienza-margot/internal/agent"
)

// defaultModel is used when a tenant has no model configured.
const defaultModel = "claude-haiku-4-5"

// Replier generates replies via the Anthropic Messages API.
type Replier struct {
	client anthropic.Client
}

// NewReplier builds a Replier authenticated with the given API key.
func NewReplier(apiKey string) *Replier {
	return &Replier{client: anthropic.NewClient(option.WithAPIKey(apiKey))}
}

// Reply sends the system prompt + conversation history to Claude and returns the
// generated text.
func (r *Replier) Reply(ctx context.Context, model, systemPrompt string, history []agent.Turn, maxTokens int) (string, error) {
	turns := normalizeTurns(history)
	if len(turns) == 0 {
		return "", nil
	}
	if model == "" {
		model = defaultModel
	}
	if maxTokens <= 0 {
		maxTokens = 400
	}

	resp, err := r.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: int64(maxTokens),
		System:    []anthropic.TextBlockParam{{Text: systemPrompt}},
		Messages:  toMessages(turns),
	})
	if err != nil {
		return "", fmt.Errorf("claude reply: %w", err)
	}
	return textFromContent(resp.Content), nil
}

func toMessages(turns []agent.Turn) []anthropic.MessageParam {
	msgs := make([]anthropic.MessageParam, 0, len(turns))
	for _, t := range turns {
		block := anthropic.NewTextBlock(t.Content)
		if t.Role == "assistant" {
			msgs = append(msgs, anthropic.NewAssistantMessage(block))
		} else {
			msgs = append(msgs, anthropic.NewUserMessage(block))
		}
	}
	return msgs
}

func textFromContent(blocks []anthropic.ContentBlockUnion) string {
	var b strings.Builder
	for _, blk := range blocks {
		if t, ok := blk.AsAny().(anthropic.TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// normalizeTurns drops empty turns and merges consecutive same-role turns, then
// trims any trailing assistant turns so the result is non-empty, strictly
// alternates user/assistant, and ends with a user turn — what the Messages API
// requires. Ending on an assistant turn would be read as an assistant prefill,
// which some models reject ("does not support assistant message prefill"); it is
// also never intended here (we reply to the last inbound message).
func normalizeTurns(history []agent.Turn) []agent.Turn {
	out := make([]agent.Turn, 0, len(history))
	for _, t := range history {
		if strings.TrimSpace(t.Content) == "" {
			continue
		}
		role := t.Role
		if role != "assistant" {
			role = "user"
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content += "\n" + t.Content
			continue
		}
		out = append(out, agent.Turn{Role: role, Content: t.Content})
	}
	// The conversation must end with a user turn (no assistant prefill).
	for len(out) > 0 && out[len(out)-1].Role == "assistant" {
		out = out[:len(out)-1]
	}
	return out
}
