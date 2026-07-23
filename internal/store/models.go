package store

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Contact is a lead/customer in a tenant's schema (LGPD: personal data).
type Contact struct {
	ID      uuid.UUID
	Phone   string
	Name    *string
	Source  string
	StageID uuid.NullUUID
	Consent bool
}

// Conversation groups messages with a contact. Billing is per AI "resposta" (see
// pipeline), so there is no service-window state on the conversation anymore.
type Conversation struct {
	ID            uuid.UUID
	ContactID     uuid.UUID
	Mode          string // "bot" | "human"
	Status        string // "open" | "closed"
	LastMessageAt *time.Time
}

// Message is a single inbound/outbound WhatsApp message.
type Message struct {
	ID             uuid.UUID `json:"id"`
	ConversationID uuid.UUID `json:"conversation_id"`
	Direction      string    `json:"direction"` // "in" | "out"
	Sender         string    `json:"sender"`    // "contact" | "bot" | "human"
	Content        string    `json:"content"`
	ProviderID     *string   `json:"provider_id"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
}

// Automation is a per-tenant inbound rule (consumed by internal/automation).
type Automation struct {
	ID       uuid.UUID       `json:"id"`
	Type     string          `json:"type"`
	Trigger  json.RawMessage `json:"trigger"`
	Action   json.RawMessage `json:"action"`
	Enabled  bool            `json:"enabled"`
	Position int32           `json:"position"`
}

// KBEntry is a knowledge-base document injected into the system prompt.
type KBEntry struct {
	ID      uuid.UUID
	Title   string
	Content string
	Tags    []string
}

// ConversationView is a conversation joined with its contact, for the API list.
type ConversationView struct {
	ID            uuid.UUID  `json:"id"`
	ContactPhone  string     `json:"contact_phone"`
	ContactName   *string    `json:"contact_name"`
	Mode          string     `json:"mode"`
	Status        string     `json:"status"`
	LastMessageAt *time.Time `json:"last_message_at"`
}

// ContactView is the CRM/funnel row (leads e clientes do tenant).
type ContactView struct {
	ID      uuid.UUID  `json:"id"`
	Phone   string     `json:"phone"`
	Name    *string    `json:"name"`
	Source  string     `json:"source"`
	StageID *uuid.UUID `json:"stage_id"`
	Consent bool       `json:"consent"`
}

// StageView is a funnel stage with the count of contacts in it.
type StageView struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Position int       `json:"position"`
	Count    int       `json:"count"`
}
