// Package agent defines the reply-generation seam used by the inbound pipeline.
// The concrete LLM-backed Replier (Claude) is wired in a later phase; until then
// the pipeline falls back to the tenant's fallback message.
package agent

import "context"

// Turn is a single conversation turn for the LLM history.
type Turn struct {
	Role    string // "user" | "assistant"
	Content string
}

// Replier generates an assistant reply from a system prompt and prior turns,
// using the given model id (per-tenant).
type Replier interface {
	Reply(ctx context.Context, model, systemPrompt string, history []Turn, maxTokens int) (string, error)
}
