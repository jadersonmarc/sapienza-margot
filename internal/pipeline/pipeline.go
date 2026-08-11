// Package pipeline is Margot's inbound conversation flow, adapted from
// rag-agente-go's whatsapp handler to run on schema-per-tenant (kit/tenancy),
// report billing/handoff via the kit event bus, and gate on the tenant's
// subscription. Every DB access runs inside a short transaction scoped to the
// tenant with WithTenant — never held across LLM or network calls.
package pipeline

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/events"
	"github.com/jadersonmarc/sapienza-kit/gating"
	"github.com/jadersonmarc/sapienza-kit/period"
	"github.com/jadersonmarc/sapienza-kit/tenancy"

	"github.com/jadersonmarc/sapienza-margot/internal/agent"
	"github.com/jadersonmarc/sapienza-margot/internal/automation"
	"github.com/jadersonmarc/sapienza-margot/internal/channel"
	"github.com/jadersonmarc/sapienza-margot/internal/store"
	"github.com/jadersonmarc/sapienza-margot/internal/transcribe"
	"github.com/jadersonmarc/sapienza-margot/internal/whatsapp"
)

// audioFallback é a resposta (canned, não-faturável) quando não há como transcrever
// o áudio — sem STT configurado ou falha na transcrição.
const audioFallback = "Ainda não consigo ouvir áudios — pode me escrever por texto? 🙏"

const (
	produto        = "margot"
	metricResposta = "resposta" // billable unit: one AI reply sent
	historyLimit   = 20
	kbMatches      = 3
)

// appointmentMarker é o token que a IA anexa à resposta quando confirma um horário
// com o cliente. O pipeline o remove antes de enviar e dispara o alerta de
// agendamento — o cliente nunca vê o token (ver appointmentInstruction).
const appointmentMarker = "[[AGENDAMENTO]]"

// appointmentInstruction é anexada ao system prompt para ensinar a IA a sinalizar
// um agendamento sem vazar o token para o cliente.
const appointmentInstruction = "\n\nQuando você confirmar um horário, data ou compromisso específico com o cliente (um agendamento), acrescente EXATAMENTE o token " + appointmentMarker + " ao final da sua mensagem. Nunca mencione, explique ou exiba esse token para o cliente — ele é um sinal interno."

// eventHandoffTriggered e eventAppointmentSignaled são eventos específicos da
// Margot anexados ao outbox da plataforma (o kit define só os tipos globais).
const (
	eventHandoffTriggered    = events.Type("HandoffTriggered")
	eventAppointmentSignaled = events.Type("AppointmentSignaled")
)

// Pipeline processes inbound WhatsApp messages for a tenant.
type Pipeline struct {
	pool        *pgxpool.Pool
	drivers     *whatsapp.Registry     // resolves the tenant's driver (evolution|meta)
	replier     agent.Replier          // nil => reply with the tenant's fallback
	transcriber transcribe.Transcriber // áudio → texto; Noop = desligado (fallback)
	gate        *gating.Client
	now         func() time.Time
}

// New builds a Pipeline. replier may be nil (fallback-only). Transcriber começa
// desligado (Noop) — use SetTranscriber para plugar um provider de STT.
func New(pool *pgxpool.Pool, drivers *whatsapp.Registry, replier agent.Replier, gate *gating.Client) *Pipeline {
	return &Pipeline{
		pool:        pool,
		drivers:     drivers,
		replier:     replier,
		transcriber: transcribe.NoopTranscriber{},
		gate:        gate,
		now:         time.Now,
	}
}

// SetTranscriber pluga o provider de transcrição de áudio (nil = desligado).
// Injetado sem mudar o construtor (espelha o padrão do Replier).
func (p *Pipeline) SetTranscriber(t transcribe.Transcriber) {
	if t == nil {
		t = transcribe.NoopTranscriber{}
	}
	p.transcriber = t
}

// Process implements whatsapp.Processor.
func (p *Pipeline) Process(ctx context.Context, ch channel.TenantChannel, in whatsapp.Inbound) error {
	// 0) Nota de voz: transcreve para texto e segue o fluxo normal. Sem STT (ou
	// falha), marca a entrada como áudio e cai num fallback por texto mais adiante.
	audioNeedsFallback := false
	if in.IsAudio && in.Text == "" {
		if text, ok := p.resolveAudioText(ctx, ch, in); ok {
			in.Text = text
		} else {
			in.Text = "[áudio]"
			audioNeedsFallback = true
		}
	}

	// 1) Persist inbound + billing, atomically, scoped to the tenant.
	conv, isNew, err := p.persistInbound(ctx, ch, in)
	if err != nil {
		return err
	}
	// Already handled in an earlier delivery of the same message (Evolution retries
	// on error/timeout). Stop here: replying again would call the model a second
	// time, send a duplicate to the contact and bill another "resposta".
	if !isNew {
		return nil
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
	// Áudio sem transcrição: responde por texto pedindo para escrever. Canned (não
	// é resposta de IA) → não fatura. Encerra aqui (não gera resposta de IA).
	if audioNeedsFallback {
		return p.sendAndRecord(ctx, ch, conv.ID, in.Phone, audioFallback, "bot", false)
	}
	// Hard cap reached: record the inbound (already persisted, shows in the console)
	// but generate nothing — the model call is the cost we are capping.
	if capped, err := p.gate.CapReached(ctx, ch.TenantID, produto, metricResposta); err != nil {
		return err
	} else if capped {
		return nil
	}

	// 2) Read decision inputs.
	count, sessionCount, autos, history, err := p.readState(ctx, ch, conv.ID)
	if err != nil {
		return err
	}

	// 3) Handoff por VOLUME da sessão atual (limite por tenant; 0 = nunca automático).
	// Conta só as mensagens desde o início da sessão do bot (bot_since) — antes usava
	// o total acumulado e escalava "sem motivo" em qualquer conversa longa.
	if ch.HandoffMax > 0 && sessionCount > int(ch.HandoffMax) {
		return p.triggerHandoff(ctx, ch, conv.ID, sessionCount)
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
	// A IA sinaliza um agendamento anexando o token à resposta: removemos o token
	// (o cliente nunca o vê) e disparamos o alerta depois de enviar a resposta.
	reply, appointment := splitAppointment(reply)
	reply = strings.TrimSpace(reply)
	if reply != "" {
		// The AI-generated reply is the billable unit ("resposta").
		if err := p.sendAndRecord(ctx, ch, conv.ID, in.Phone, reply, "bot", true); err != nil {
			return err
		}
	} else if !appointment {
		return nil
	}
	if appointment {
		return p.signalAppointment(ctx, ch, conv.ID)
	}
	return nil
}

// splitAppointment removes every appointmentMarker occurrence from the reply and
// reports whether the AI signaled an appointment.
func splitAppointment(reply string) (string, bool) {
	if !strings.Contains(reply, appointmentMarker) {
		return reply, false
	}
	return strings.ReplaceAll(reply, appointmentMarker, ""), true
}

// signalAppointment flags the conversation for attention (console + e-mail alert)
// and emits AppointmentSignaled. It does NOT hand off — the bot keeps the
// conversation; the owner is only notified that a time was confirmed.
func (p *Pipeline) signalAppointment(ctx context.Context, ch channel.TenantChannel, convID uuid.UUID) error {
	return p.withTenant(ctx, ch.TenantID, func(tx pgx.Tx) error {
		if err := store.SetNeedsAttention(ctx, tx, convID, true, "A IA confirmou um horário — confira o agendamento"); err != nil {
			return err
		}
		_, err := events.Publish(ctx, tx, eventAppointmentSignaled, ch.TenantID, produto, map[string]any{
			"tenant_id": ch.TenantID.String(), "conversation_id": convID.String(),
		})
		return err
	})
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

// persistInbound upserts the contact/conversation and stores the inbound message.
// Inbound is free and never billed — billing happens on the AI reply (sendAndRecord).
//
// The bool reports whether this delivery is new. False means Evolution redelivered
// a message we already stored (same provider_id), and the caller must not act on
// it again — see Process.
func (p *Pipeline) persistInbound(ctx context.Context, ch channel.TenantChannel, in whatsapp.Inbound) (store.Conversation, bool, error) {
	var conv store.Conversation
	var isNew bool
	err := p.withTenant(ctx, ch.TenantID, func(tx pgx.Tx) error {
		contact, err := store.UpsertContact(ctx, tx, in.Phone, optional(in.PushName))
		if err != nil {
			return err
		}
		conv, err = store.GetOrCreateConversation(ctx, tx, contact.ID)
		if err != nil {
			return err
		}
		if err := store.TouchConversation(ctx, tx, conv.ID); err != nil {
			return err
		}
		_, isNew, err = store.InsertMessageIfNew(ctx, tx, store.Message{
			ConversationID: conv.ID, Direction: "in", Sender: "contact",
			Content: in.Text, ProviderID: optional(in.ProviderID),
		})
		return err
	})
	return conv, isNew, err
}

func (p *Pipeline) readState(ctx context.Context, ch channel.TenantChannel, convID uuid.UUID) (int, int, []store.Automation, []store.Message, error) {
	var count, sessionCount int
	var autos []store.Automation
	var history []store.Message
	err := p.withTenant(ctx, ch.TenantID, func(tx pgx.Tx) error {
		var err error
		if count, err = store.CountMessages(ctx, tx, convID); err != nil {
			return err
		}
		if sessionCount, err = store.CountSessionMessages(ctx, tx, convID); err != nil {
			return err
		}
		if autos, err = store.ListAutomations(ctx, tx); err != nil {
			return err
		}
		history, err = store.ListRecentMessages(ctx, tx, convID, historyLimit)
		return err
	})
	return count, sessionCount, autos, history, err
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
		// Destaque no console + alerta: a conversa precisa de humano, com o motivo.
		if err := store.SetNeedsAttention(ctx, tx, convID, true, "Limite de mensagens da sessão atingido"); err != nil {
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
		// Automation replies are canned (not AI-generated) → not billable.
		if err := p.sendAndRecord(ctx, ch, convID, in.Phone, dec.Reply, "bot", false); err != nil {
			return err
		}
	}
	if dec.Handoff {
		return p.withTenant(ctx, ch.TenantID, func(tx pgx.Tx) error {
			if err := store.SetConversationMode(ctx, tx, convID, "human"); err != nil {
				return err
			}
			return store.SetNeedsAttention(ctx, tx, convID, true, "Automação encaminhou para atendimento humano")
		})
	}
	return nil
}

// resolveAudioText busca os bytes do áudio (via driver) e transcreve. Devolve
// (texto, true) no sucesso; ("", false) quando não há STT ou algo falha — o
// chamador cai no fallback por texto. Best-effort: só loga as falhas.
func (p *Pipeline) resolveAudioText(ctx context.Context, ch channel.TenantChannel, in whatsapp.Inbound) (string, bool) {
	if p.transcriber == nil || !p.transcriber.Configured() {
		return "", false
	}
	data, mime, err := p.drivers.For(ch.Driver).MediaBase64(ctx, ch.EvolutionInstance, whatsapp.MediaKey{
		ID: in.ProviderID, RemoteJid: in.Phone + "@s.whatsapp.net", FromMe: in.FromMe,
	})
	if err != nil {
		log.Printf("[pipeline] falha ao buscar áudio (tenant %s): %v", ch.TenantID, err)
		return "", false
	}
	text, err := p.transcriber.Transcribe(ctx, data, mime)
	if err != nil {
		log.Printf("[pipeline] falha ao transcrever áudio (tenant %s): %v", ch.TenantID, err)
		return "", false
	}
	if text = strings.TrimSpace(text); text == "" {
		return "", false
	}
	return text, true
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
	// Ensina a IA a sinalizar agendamentos (token removido antes de enviar).
	prompt += appointmentInstruction
	return p.replier.Reply(ctx, ch.AIModel, prompt, toTurns(history), int(ch.MaxTokens))
}

// sendAndRecord sends via the tenant's driver (outside any tx), records the
// outbound, and — when billable (an AI-generated reply) — emits one
// UsageRecorded{metric:"resposta"} in the same transaction as the outbound insert.
func (p *Pipeline) sendAndRecord(ctx context.Context, ch channel.TenantChannel, convID uuid.UUID, phone, text, sender string, billable bool) error {
	sentID, err := p.drivers.For(ch.Driver).SendText(ctx, ch.EvolutionInstance, phone, text)
	if err != nil {
		return fmt.Errorf("send reply: %w", err)
	}
	return p.withTenant(ctx, ch.TenantID, func(tx pgx.Tx) error {
		if _, err := store.InsertMessage(ctx, tx, store.Message{
			ConversationID: convID, Direction: "out", Sender: sender,
			Content: text, ProviderID: optional(sentID),
		}); err != nil {
			return err
		}
		if !billable {
			return nil
		}
		// Billable "resposta": one UsageRecorded per AI reply sent, appended to the
		// platform outbox in the SAME transaction (transactional outbox).
		per := period.Current(p.now())
		_, err := events.Publish(ctx, tx, events.TypeUsageRecorded, ch.TenantID, produto, events.UsageRecorded{
			TenantID: ch.TenantID, Produto: produto, Metric: "resposta", Count: 1, Period: per,
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
