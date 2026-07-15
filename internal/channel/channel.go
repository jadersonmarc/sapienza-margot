// Package channel resolves an Evolution instance name to its tenant's channel
// config (tenant id, agent config, decrypted credentials), backed by the
// product-global margot.tenant_channels table with a short TTL cache — the same
// shape as rag-agente-go/internal/tenant/resolver_db.go, adapted to Margot.
package channel

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-margot/internal/secrets"
)

// TenantChannel is the per-tenant WhatsApp channel + agent config.
type TenantChannel struct {
	TenantID                 uuid.UUID
	EvolutionInstance        string
	WhatsappNumber           string
	Driver                   string // "evolution" (default) | "meta"
	DedicatedNumberConfirmed bool   // onboarding requirement before activating Evolution
	APIURL                   string // per-tenant override; empty => use global env
	APIKey                   string // decrypted; empty => use global env
	SystemPrompt             string
	Tone                     string
	Fallback                 string
	MaxTokens                int32
	AIModel                  string
}

// Loader reads a channel from Postgres.
type Loader struct {
	pool   *pgxpool.Pool
	cipher *secrets.Cipher
}

// NewLoader builds a Loader; cipher may be nil if no encrypted fields are used.
func NewLoader(pool *pgxpool.Pool, cipher *secrets.Cipher) *Loader {
	return &Loader{pool: pool, cipher: cipher}
}

// ByInstance loads the channel for an Evolution instance from margot.tenant_channels.
func (l *Loader) ByInstance(ctx context.Context, instance string) (TenantChannel, error) {
	var c TenantChannel
	var apiURL, apiKeyEnc *string
	err := l.pool.QueryRow(ctx, `
		SELECT tenant_id, evolution_instance, COALESCE(whatsapp_number, ''),
		       driver, dedicated_number_confirmed,
		       api_url, api_key_enc, system_prompt, tone, fallback, max_tokens, ai_model
		  FROM margot.tenant_channels WHERE evolution_instance = $1`, instance,
	).Scan(&c.TenantID, &c.EvolutionInstance, &c.WhatsappNumber,
		&c.Driver, &c.DedicatedNumberConfirmed,
		&apiURL, &apiKeyEnc, &c.SystemPrompt, &c.Tone, &c.Fallback, &c.MaxTokens, &c.AIModel)
	if err == pgx.ErrNoRows {
		return TenantChannel{}, fmt.Errorf("no channel for instance %q", instance)
	}
	if err != nil {
		return TenantChannel{}, fmt.Errorf("load channel %q: %w", instance, err)
	}
	if apiURL != nil {
		c.APIURL = *apiURL
	}
	if apiKeyEnc != nil && *apiKeyEnc != "" {
		if l.cipher == nil {
			return TenantChannel{}, fmt.Errorf("channel %q has an encrypted key but no cipher configured", instance)
		}
		key, err := l.cipher.Decrypt(*apiKeyEnc)
		if err != nil {
			return TenantChannel{}, fmt.Errorf("decrypt api key for %q: %w", instance, err)
		}
		c.APIKey = key
	}
	return c, nil
}

// resolver is the loader interface (kept small for testing).
type resolver interface {
	ByInstance(ctx context.Context, instance string) (TenantChannel, error)
}

// Resolver caches channels by instance for a short TTL to avoid a DB hit per message.
type Resolver struct {
	loader resolver
	ttl    time.Duration

	mu    sync.RWMutex
	cache map[string]entry
}

type entry struct {
	ch TenantChannel
	at time.Time
}

// NewResolver builds a Resolver over the loader with a 60s TTL.
func NewResolver(l resolver) *Resolver {
	return &Resolver{loader: l, ttl: 60 * time.Second, cache: make(map[string]entry)}
}

// ByInstance returns the channel, using the cache when fresh.
func (r *Resolver) ByInstance(ctx context.Context, instance string) (TenantChannel, error) {
	r.mu.RLock()
	if e, ok := r.cache[instance]; ok && time.Since(e.at) < r.ttl {
		r.mu.RUnlock()
		return e.ch, nil
	}
	r.mu.RUnlock()

	ch, err := r.loader.ByInstance(ctx, instance)
	if err != nil {
		return TenantChannel{}, err
	}
	r.mu.Lock()
	r.cache[instance] = entry{ch: ch, at: time.Now()}
	r.mu.Unlock()
	return ch, nil
}

// Invalidate drops the cached entry for an instance (after a config update).
func (r *Resolver) Invalidate(instance string) {
	r.mu.Lock()
	delete(r.cache, instance)
	r.mu.Unlock()
}
