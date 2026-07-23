// Package store holds the tenant-scoped repositories for Margot's conversation
// data. Every method takes a DBTX that MUST already be scoped to the tenant via
// kit/tenancy.WithTenant (search_path = tenant_<id>, public). Queries therefore
// carry no tenant_id and no schema prefix — the schema is the isolation boundary.
// Hand-written pgx for reviewability (no sqlc), mirroring rag-agente-go.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DBTX is the subset of pgx used by the store — satisfied by pgx.Tx (and pools).
// In practice callers pass the WithTenant-scoped transaction.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// UpsertContact inserts or updates a contact by phone.
func UpsertContact(ctx context.Context, tx DBTX, phone string, name *string) (Contact, error) {
	var c Contact
	err := tx.QueryRow(ctx, `
		INSERT INTO contacts (phone, name)
		VALUES ($1, $2)
		ON CONFLICT (phone) DO UPDATE
		  SET name = COALESCE(EXCLUDED.name, contacts.name), updated_at = now()
		RETURNING id, phone, name, source, stage_id, consent`,
		phone, name,
	).Scan(&c.ID, &c.Phone, &c.Name, &c.Source, &c.StageID, &c.Consent)
	if err != nil {
		return Contact{}, fmt.Errorf("upsert contact: %w", err)
	}
	return c, nil
}

// GetOrCreateConversation returns the contact's open conversation, creating one
// (in bot mode) if none exists.
func GetOrCreateConversation(ctx context.Context, tx DBTX, contactID uuid.UUID) (Conversation, error) {
	var c Conversation
	err := tx.QueryRow(ctx, `
		INSERT INTO conversations (contact_id)
		VALUES ($1)
		ON CONFLICT (contact_id) DO UPDATE SET contact_id = EXCLUDED.contact_id
		RETURNING id, contact_id, mode, status, last_message_at`,
		contactID,
	).Scan(&c.ID, &c.ContactID, &c.Mode, &c.Status, &c.LastMessageAt)
	if err != nil {
		return Conversation{}, fmt.Errorf("get/create conversation: %w", err)
	}
	return c, nil
}

// TouchConversation updates the conversation's last message time.
func TouchConversation(ctx context.Context, tx DBTX, conversationID uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE conversations SET last_message_at = now() WHERE id = $1`, conversationID)
	if err != nil {
		return fmt.Errorf("touch conversation: %w", err)
	}
	return nil
}

// SetConversationMode switches a conversation between "bot" and "human".
func SetConversationMode(ctx context.Context, tx DBTX, id uuid.UUID, mode string) error {
	_, err := tx.Exec(ctx, `UPDATE conversations SET mode = $2 WHERE id = $1`, id, mode)
	if err != nil {
		return fmt.Errorf("set conversation mode: %w", err)
	}
	return nil
}

// InsertMessage appends a message to a conversation.
func InsertMessage(ctx context.Context, tx DBTX, m Message) (Message, error) {
	if m.Status == "" {
		m.Status = "sent"
	}
	err := tx.QueryRow(ctx, `
		INSERT INTO messages (conversation_id, direction, sender, content, provider_id, status)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at`,
		m.ConversationID, m.Direction, m.Sender, m.Content, m.ProviderID, m.Status,
	).Scan(&m.ID, &m.CreatedAt)
	if err != nil {
		return Message{}, fmt.Errorf("insert message: %w", err)
	}
	return m, nil
}

// InsertMessageIfNew appends a message unless one with the same provider_id is
// already stored, reporting whether it inserted.
//
// This is the idempotency guard for inbound webhooks: Evolution retries on error
// or timeout, and reprocessing a message means another model call, another reply
// to the contact and another billed "resposta". Messages without a provider id
// (provider_id NULL) never dedup — the partial unique index skips them.
func InsertMessageIfNew(ctx context.Context, tx DBTX, m Message) (Message, bool, error) {
	if m.Status == "" {
		m.Status = "sent"
	}
	err := tx.QueryRow(ctx, `
		INSERT INTO messages (conversation_id, direction, sender, content, provider_id, status)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (provider_id) WHERE provider_id IS NOT NULL DO NOTHING
		RETURNING id, created_at`,
		m.ConversationID, m.Direction, m.Sender, m.Content, m.ProviderID, m.Status,
	).Scan(&m.ID, &m.CreatedAt)
	// DO NOTHING returns no row: already seen, not an error. Letting ErrNoRows
	// escape would 500 the webhook and make Evolution retry — the very loop this
	// guard exists to stop.
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, false, nil
	}
	if err != nil {
		return Message{}, false, fmt.Errorf("insert message if new: %w", err)
	}
	return m, true, nil
}

// ListRecentMessages returns the last `limit` messages of a conversation, oldest first.
func ListRecentMessages(ctx context.Context, tx DBTX, conversationID uuid.UUID, limit int) ([]Message, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, conversation_id, direction, sender, content, provider_id, status, created_at
		  FROM (
		    SELECT * FROM messages WHERE conversation_id = $1 ORDER BY created_at DESC LIMIT $2
		  ) sub ORDER BY created_at ASC`,
		conversationID, limit)
	if err != nil {
		return nil, fmt.Errorf("list recent messages: %w", err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Direction, &m.Sender,
			&m.Content, &m.ProviderID, &m.Status, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountMessages returns the number of messages in a conversation.
func CountMessages(ctx context.Context, tx DBTX, conversationID uuid.UUID) (int, error) {
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM messages WHERE conversation_id = $1`, conversationID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count messages: %w", err)
	}
	return n, nil
}

// InsertHandoff records a handoff for a conversation.
func InsertHandoff(ctx context.Context, tx DBTX, conversationID uuid.UUID, reason string) error {
	_, err := tx.Exec(ctx, `INSERT INTO handoffs (conversation_id, reason) VALUES ($1, $2)`, conversationID, reason)
	if err != nil {
		return fmt.Errorf("insert handoff: %w", err)
	}
	return nil
}

// ListAutomations returns the tenant's automations ordered by position.
func ListAutomations(ctx context.Context, tx DBTX) ([]Automation, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, type, trigger, action, enabled, position FROM automations ORDER BY position`)
	if err != nil {
		return nil, fmt.Errorf("list automations: %w", err)
	}
	defer rows.Close()
	var out []Automation
	for rows.Next() {
		var a Automation
		if err := rows.Scan(&a.ID, &a.Type, &a.Trigger, &a.Action, &a.Enabled, &a.Position); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListConversations returns the tenant's conversations joined with contacts,
// most recently active first.
func ListConversations(ctx context.Context, tx DBTX, limit int) ([]ConversationView, error) {
	rows, err := tx.Query(ctx, `
		SELECT c.id, ct.phone, ct.name, c.mode, c.status, c.last_message_at
		  FROM conversations c
		  JOIN contacts ct ON ct.id = c.contact_id
		 ORDER BY c.last_message_at DESC NULLS LAST
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer rows.Close()
	var out []ConversationView
	for rows.Next() {
		var v ConversationView
		if err := rows.Scan(&v.ID, &v.ContactPhone, &v.ContactName, &v.Mode, &v.Status, &v.LastMessageAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ListContacts returns leads/customers of the tenant, optionally filtered by stage.
func ListContacts(ctx context.Context, tx DBTX, stageID *uuid.UUID, limit int) ([]ContactView, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, phone, name, source, stage_id, consent
		  FROM contacts
		 WHERE ($1::uuid IS NULL OR stage_id = $1)
		 ORDER BY updated_at DESC
		 LIMIT $2`, stageID, limit)
	if err != nil {
		return nil, fmt.Errorf("list contacts: %w", err)
	}
	defer rows.Close()
	var out []ContactView
	for rows.Next() {
		var v ContactView
		var stage uuid.NullUUID
		if err := rows.Scan(&v.ID, &v.Phone, &v.Name, &v.Source, &stage, &v.Consent); err != nil {
			return nil, err
		}
		if stage.Valid {
			id := stage.UUID
			v.StageID = &id
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ListPipelineStages returns the funnel stages with the count of contacts in each.
func ListPipelineStages(ctx context.Context, tx DBTX) ([]StageView, error) {
	rows, err := tx.Query(ctx, `
		SELECT s.id, s.name, s.position, COUNT(c.id)
		  FROM pipeline_stages s
		  LEFT JOIN contacts c ON c.stage_id = s.id
		 GROUP BY s.id, s.name, s.position
		 ORDER BY s.position, s.name`)
	if err != nil {
		return nil, fmt.Errorf("list pipeline stages: %w", err)
	}
	defer rows.Close()
	var out []StageView
	for rows.Next() {
		var v StageView
		if err := rows.Scan(&v.ID, &v.Name, &v.Position, &v.Count); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// UpdateContact edits a lead's name, funnel stage and consent (consent_at is set
// on the first opt-in). Scoped to the tenant by the caller's WithTenant.
func UpdateContact(ctx context.Context, tx DBTX, id uuid.UUID, name *string, stageID uuid.NullUUID, consent bool) error {
	_, err := tx.Exec(ctx, `
		UPDATE contacts
		   SET name = $2, stage_id = $3, consent = $4,
		       consent_at = CASE WHEN $4 THEN COALESCE(consent_at, now()) ELSE consent_at END,
		       updated_at = now()
		 WHERE id = $1`, id, name, stageID, consent)
	if err != nil {
		return fmt.Errorf("update contact: %w", err)
	}
	return nil
}

// DeleteContact removes a lead and cascades to its conversations/messages (LGPD).
func DeleteContact(ctx context.Context, tx DBTX, id uuid.UUID) error {
	_, err := tx.Exec(ctx, `DELETE FROM contacts WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete contact: %w", err)
	}
	return nil
}

// SearchKnowledge returns up to `limit` KB entries matching the query text
// (simple case-insensitive match on title/content/tags). This is the "injeção
// simples" KB — pgvector semantic retrieval is a fast-follow.
func SearchKnowledge(ctx context.Context, tx DBTX, query string, limit int) ([]KBEntry, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, title, content, tags FROM knowledge_base
		 WHERE title ILIKE '%' || $1 || '%'
		    OR content ILIKE '%' || $1 || '%'
		    OR EXISTS (SELECT 1 FROM unnest(tags) t WHERE t ILIKE '%' || $1 || '%')
		 LIMIT $2`, query, limit)
	if err != nil {
		return nil, fmt.Errorf("search knowledge: %w", err)
	}
	defer rows.Close()
	var out []KBEntry
	for rows.Next() {
		var e KBEntry
		if err := rows.Scan(&e.ID, &e.Title, &e.Content, &e.Tags); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
