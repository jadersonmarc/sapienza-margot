// Package pipeline is Margot's inbound conversation flow, adapted from
// rag-agente-go's whatsapp handler to run on schema-per-tenant (kit/tenancy),
// report billing/handoff via the kit event bus, and gate on the tenant's
// subscription. Every DB access runs inside a short transaction scoped to the
// tenant with WithTenant — never held across LLM or network calls.
package pipeline

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/events"
	"github.com/jadersonmarc/sapienza-kit/gating"
	"github.com/jadersonmarc/sapienza-kit/tenancy"

	"github.com/jadersonmarc/sapienza-margot/internal/agent"
	"github.com/jadersonmarc/sapienza-margot/internal/automation"
	"github.com/jadersonmarc/sapienza-margot/internal/channel"
	"github.com/jadersonmarc/sapienza-margot/internal/store"
	"github.com/jadersonmarc/sapienza-margot/internal/whatsapp"
)

const (
	produto           = "margot"
	defaultHandoffMax = 15
	serviceWindow     = 24 * time.Hour
	historyLimit      = 20
	kbMatches         = 3
)

// eventHandoffTriggered is a Margot-specific event appended to the platform
// outbox (the kit defines only platform-wide event types).
const eventHandoffTriggered = events.Type("HandoffTriggered")

// Pipeline processes inbound WhatsApp messages for a tenant.
type Pipeline struct {
	pool    *pgxpool.Pool
	sender  whatsapp.Sender
	replier agent.Replier // nil => reply with the tenant's fallback
	gate    *gating.Client
	rules   *rulesCache
	now     func() time.Time
}

// New builds a Pipeline. replier may be nil (fallback-only).
func New(pool *pgxpool.Pool, sender whatsapp.Sender, replier agent.Replier, gate *gating.Client) *Pipeline {
	return &Pipeline{
		pool:    pool,
		sender:  sender,
		replier: replier,
		gate:    gate,
		rules:   &rulesCache{pool: pool, ttl: 60 * time.Second},
		now:     time.Now,
	}
}

// Process implements whatsapp.Processor.
func (p *Pipeline) Process(ctx context.Context, ch channel.TenantChannel, in whatsapp.Inbound) error {
	// 1) Persist inbound + billing, atomically, scoped to the tenant.
	conv, err := p.persistInbound(ctx, ch, in)
	if err != nil {
		return err
	}
	// Human owns the conversation: record only.
	if conv.Mode != "bot" {
		return nil
	}
	// Subscription gate (no user in context): inactive => no bot activity.
	if ok, err := p.gate.TenantCanOperate(ctx, ch.TenantID, produto); err != nil {
		return err
	} else if !ok {
		return nil
	}

	// 2) Read decision inputs.
	count, autos, history, err := p.readState(ctx, ch, conv.ID)
	if err != nil {
		return err
	}

	// 3) Handoff rule (from product_rules; default 15).
	handoffMax, err := p.rules.handoffMax(ctx)
	if err != nil {
		return err
	}
	if count > handoffMax {
		return p.triggerHandoff(ctx, ch, conv.ID, count)
	}

	// 4) Automations may short-circuit the bot.
	rules, err := automation.RulesFrom(autos)
	if err != nil {
		return err
	}
	dec := automation.Evaluate(rules, automation.Input{Text: in.Text, FirstMessage: count == 1, Now: p.now()})
	if dec.Triggered {
		return p.applyAutomation(ctx, ch, conv.ID, in, dec)
	}

	// 5) Generate the bot reply (LLM outside any transaction).
	reply, err := p.generateReply(ctx, ch, in.Text, history)
	if err != nil {
		return err
	}
	if strings.TrimSpace(reply) == "" {
		return nil
	}
	return p.sendAndRecord(ctx, ch, conv.ID, in.Phone, reply, "bot")
}

// withTenant runs fn inside a transaction scoped to the tenant's schema.
func (p *Pipeline) withTenant(ctx context.Context, tenantID uuid.UUID, fn func(tx pgx.Tx) error) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tenancy.WithTenant(ctx, tx, tenantID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// persistInbound upserts the contact/conversation, opens or extends the 24h
// window (emitting UsageRecorded once per new window), and stores the inbound.
func (p *Pipeline) persistInbound(ctx context.Context, ch channel.TenantChannel, in whatsapp.Inbound) (store.Conversation, error) {
	var conv store.Conversation
	var newWindow bool
	err := p.withTenant(ctx, ch.TenantID, func(tx pgx.Tx) error {
		contact, err := store.UpsertContact(ctx, tx, in.Phone, optional(in.PushName))
		if err != nil {
			return err
		}
		conv, err = store.GetOrCreateConversation(ctx, tx, contact.ID)
		if err != nil {
			return err
		}
		newWindow = conv.WindowStartedAt == nil || p.now().Sub(*conv.WindowStartedAt) > serviceWindow
		if newWindow {
			if err := store.StartWindow(ctx, tx, conv.ID); err != nil {
				return err
			}
		} else if err := store.TouchConversation(ctx, tx, conv.ID); err != nil {
			return err
		}
		if _, err := store.InsertMessage(ctx, tx, store.Message{
			ConversationID: conv.ID, Direction: "in", Sender: "contact",
			Content: in.Text, ProviderID: optional(in.ProviderID),
		}); err != nil {
			return err
		}
		// Billable "conversa": one UsageRecorded per new 24h window, appended to
		// the platform outbox in the SAME transaction (transactional outbox).
		if newWindow {
			period := p.now().UTC().Format("2006-01")
			if _, err := events.Publish(ctx, tx, events.TypeUsageRecorded, ch.TenantID, produto, events.UsageRecorded{
				TenantID: ch.TenantID, Produto: produto, Metric: "conversa", Count: 1, Period: period,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	return conv, err
}

func (p *Pipeline) readState(ctx context.Context, ch channel.TenantChannel, convID uuid.UUID) (int, []store.Automation, []store.Message, error) {
	var count int
	var autos []store.Automation
	var history []store.Message
	err := p.withTenant(ctx, ch.TenantID, func(tx pgx.Tx) error {
		var err error
		if count, err = store.CountMessages(ctx, tx, convID); err != nil {
			return err
		}
		if autos, err = store.ListAutomations(ctx, tx); err != nil {
			return err
		}
		history, err = store.ListRecentMessages(ctx, tx, convID, historyLimit)
		return err
	})
	return count, autos, history, err
}

// triggerHandoff records the handoff, flips the conversation to human, and emits
// HandoffTriggered — all in one tenant-scoped transaction. Stops auto-replies.
func (p *Pipeline) triggerHandoff(ctx context.Context, ch channel.TenantChannel, convID uuid.UUID, count int) error {
	return p.withTenant(ctx, ch.TenantID, func(tx pgx.Tx) error {
		if err := store.InsertHandoff(ctx, tx, convID, "max_mensagens"); err != nil {
			return err
		}
		if err := store.SetConversationMode(ctx, tx, convID, "human"); err != nil {
			return err
		}
		_, err := events.Publish(ctx, tx, eventHandoffTriggered, ch.TenantID, produto, map[string]any{
			"tenant_id": ch.TenantID.String(), "conversation_id": convID.String(),
			"reason": "max_mensagens", "count": count,
		})
		return err
	})
}

func (p *Pipeline) applyAutomation(ctx context.Context, ch channel.TenantChannel, convID uuid.UUID, in whatsapp.Inbound, dec automation.Decision) error {
	if dec.Reply != "" {
		if err := p.sendAndRecord(ctx, ch, convID, in.Phone, dec.Reply, "bot"); err != nil {
			return err
		}
	}
	if dec.Handoff {
		return p.withTenant(ctx, ch.TenantID, func(tx pgx.Tx) error {
			return store.SetConversationMode(ctx, tx, convID, "human")
		})
	}
	return nil
}

// generateReply builds the system prompt (config + KB injection) and calls the
// Replier. With no Replier it returns the tenant fallback.
func (p *Pipeline) generateReply(ctx context.Context, ch channel.TenantChannel, text string, history []store.Message) (string, error) {
	if p.replier == nil {
		return ch.Fallback, nil
	}
	prompt := ch.SystemPrompt
	// KB: inject matching entries into the system prompt (simple retrieval).
	var kb []store.KBEntry
	if err := p.withTenant(ctx, ch.TenantID, func(tx pgx.Tx) error {
		var err error
		kb, err = store.SearchKnowledge(ctx, tx, text, kbMatches)
		return err
	}); err != nil {
		return "", err
	}
	if len(kb) > 0 {
		var b strings.Builder
		b.WriteString(prompt)
		b.WriteString("\n\nBase de conhecimento (use quando relevante):\n")
		for _, e := range kb {
			fmt.Fprintf(&b, "- %s: %s\n", e.Title, e.Content)
		}
		prompt = b.String()
	}
	return p.replier.Reply(ctx, ch.AIModel, prompt, toTurns(history), int(ch.MaxTokens))
}

// sendAndRecord sends via Evolution (outside any tx), then records the outbound.
func (p *Pipeline) sendAndRecord(ctx context.Context, ch channel.TenantChannel, convID uuid.UUID, phone, text, sender string) error {
	sentID, err := p.sender.SendText(ctx, ch.EvolutionInstance, phone, text)
	if err != nil {
		return fmt.Errorf("send reply: %w", err)
	}
	return p.withTenant(ctx, ch.TenantID, func(tx pgx.Tx) error {
		_, err := store.InsertMessage(ctx, tx, store.Message{
			ConversationID: convID, Direction: "out", Sender: sender,
			Content: text, ProviderID: optional(sentID),
		})
		return err
	})
}

func toTurns(msgs []store.Message) []agent.Turn {
	turns := make([]agent.Turn, 0, len(msgs))
	for _, m := range msgs {
		role := "user"
		if m.Direction == "out" {
			role = "assistant"
		}
		turns = append(turns, agent.Turn{Role: role, Content: m.Content})
	}
	return turns
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// rulesCache caches product_rules (read-only from public) for a short TTL.
type rulesCache struct {
	pool *pgxpool.Pool
	ttl  time.Duration

	mu     sync.Mutex
	at     time.Time
	handMx int
	loaded bool
}

func (c *rulesCache) handoffMax(ctx context.Context) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded && time.Since(c.at) < c.ttl {
		return c.handMx, nil
	}
	var v *int
	err := c.pool.QueryRow(ctx,
		`SELECT (rules->>'handoff_max_mensagens')::int FROM public.product_rules WHERE produto = $1`, produto,
	).Scan(&v)
	if err != nil && err != pgx.ErrNoRows {
		return 0, fmt.Errorf("read handoff_max: %w", err)
	}
	c.handMx = defaultHandoffMax
	if v != nil {
		c.handMx = *v
	}
	c.at = time.Now()
	c.loaded = true
	return c.handMx, nil
}
