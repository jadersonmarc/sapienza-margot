// Package events defines the Sapienza event contract and a Postgres-backed
// transactional outbox (event_outbox in public + pg_notify) with a per-consumer
// cursor. The control plane (sapienza-core, TypeScript) and Go products publish
// and consume the same JSON envelopes over one Postgres — no external broker.
//
// Golden rule: only the core writes to public. The outbox table lives in public
// and is written by the core (and by products only when reporting usage, which
// the core sanctions). Consumer cursors live in a separate "bus" schema so
// products never write to public for bookkeeping.
package events

import (
	"time"

	"github.com/google/uuid"
)

// Type enumerates the platform event types. Values must stay in sync with the
// core (TypeScript) that writes the same strings into event_outbox.type.
type Type string

const (
	TypeTenantProvisioned     Type = "TenantProvisioned"
	TypeSubscriptionActivated Type = "SubscriptionActivated"
	TypeSubscriptionChanged   Type = "SubscriptionChanged"
	TypeUsageRecorded         Type = "UsageRecorded"
	TypeTierExceeded          Type = "TierExceeded"
	TypeInvoiceIssued         Type = "InvoiceIssued"
)

// NotifyChannel is the pg_notify channel consumers LISTEN on for wakeups.
const NotifyChannel = "sapienza_events"

// Envelope is one row of the outbox. Payload is the type-specific JSON body.
type Envelope struct {
	ID        int64           `json:"id"`
	Type      Type            `json:"type"`
	TenantID  uuid.UUID       `json:"tenant_id"`
	Produto   string          `json:"produto,omitempty"`
	Payload   map[string]any  `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

// ── Payload structs (the contract). Each mirrors what the core writes. ────────

// TenantProvisioned: core created an empty tenant_<id> schema.
type TenantProvisioned struct {
	TenantID uuid.UUID `json:"tenant_id"`
	Schema   string    `json:"schema"`
}

// SubscriptionActivated: a tenant activated a product subscription at a tier.
type SubscriptionActivated struct {
	TenantID uuid.UUID `json:"tenant_id"`
	Produto  string    `json:"produto"`
	Tier     string    `json:"tier"`
}

// SubscriptionChanged: tier or status of a subscription changed.
type SubscriptionChanged struct {
	TenantID uuid.UUID `json:"tenant_id"`
	Produto  string    `json:"produto"`
	FromTier string    `json:"from_tier"`
	ToTier   string    `json:"to_tier"`
	Status   string    `json:"status"`
}

// UsageRecorded: a product reported a billable unit (conversa, peca, ...).
type UsageRecorded struct {
	TenantID uuid.UUID `json:"tenant_id"`
	Produto  string    `json:"produto"`
	Metric   string    `json:"metric"`
	Count    int       `json:"count"`
	Period   string    `json:"period"` // "YYYY-MM"
}

// TierExceeded: usage passed the tier's included amount.
type TierExceeded struct {
	TenantID  uuid.UUID `json:"tenant_id"`
	Produto   string    `json:"produto"`
	Metric    string    `json:"metric"`
	Count     int       `json:"count"`
	Incluso   int       `json:"incluso"`
	Period    string    `json:"period"`
}

// InvoiceIssued: the core closed a monthly invoice for a tenant.
type InvoiceIssued struct {
	TenantID uuid.UUID `json:"tenant_id"`
	Period   string    `json:"period"`
	TotalBRL float64   `json:"total_brl"`
}
