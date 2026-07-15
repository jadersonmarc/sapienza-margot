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
	ID             uuid.UUID
	ConversationID uuid.UUID
	Direction      string // "in" | "out"
	Sender         string // "contact" | "bot" | "human"
	Content        string
	ProviderID     *string
	Status         string
	CreatedAt      time.Time
}

// Automation is a per-tenant inbound rule (consumed by internal/automation).
type Automation struct {
	ID       uuid.UUID
	Type     string
	Trigger  json.RawMessage
	Action   json.RawMessage
	Enabled  bool
	Position int32
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
	ID            uuid.UUID
	ContactPhone  string
	ContactName   *string
	Mode          string
	Status        string
	LastMessageAt *time.Time
}
