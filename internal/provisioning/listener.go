// Package provisioning applies Margot's per-tenant migrations when a tenant
// activates the margot subscription. It consumes the platform outbox
// (SubscriptionActivated{margot}) via the kit and applies the tenant migrations
// with kit/tenancy.MigrationRunner. A boot catch-up covers tenants that
// subscribed before this process started.
package provisioning

import (
	"context"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/events"
	"github.com/jadersonmarc/sapienza-kit/tenancy"

	"github.com/jadersonmarc/sapienza-margot/db/migrations"
)

const produto = "margot"

// Listener applies tenant migrations driven by subscription events.
type Listener struct {
	pool     *pgxpool.Pool
	consumer *events.Consumer
	runner   *tenancy.MigrationRunner
	interval time.Duration
}

// New builds a provisioning listener.
func New(pool *pgxpool.Pool) *Listener {
	return &Listener{
		pool:     pool,
		consumer: events.NewConsumer(pool, "margot-provisioning"),
		runner:   tenancy.NewMigrationRunner(pool, migrations.Tenant),
		interval: 5 * time.Second,
	}
}

// CatchUp applies tenant migrations to every tenant with an active margot
// subscription (idempotent). Run once at boot.
func (l *Listener) CatchUp(ctx context.Context) error {
	rows, err := l.pool.Query(ctx,
		`SELECT tenant_id FROM public.subscriptions WHERE produto = $1 AND status = 'active'`, produto)
	if err != nil {
		return err
	}
	defer rows.Close()

	var tenantIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		tenantIDs = append(tenantIDs, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range tenantIDs {
		if err := l.applyByString(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// Run polls the outbox and applies migrations on SubscriptionActivated{margot}.
// Blocks until ctx is done.
func (l *Listener) Run(ctx context.Context) {
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		if err := l.drain(ctx); err != nil {
			log.Printf("provisioning: drain: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (l *Listener) drain(ctx context.Context) error {
	batch, err := l.consumer.Fetch(ctx, 100)
	if err != nil {
		return err
	}
	if len(batch) == 0 {
		return nil
	}
	for _, e := range batch {
		if e.Type == events.TypeSubscriptionActivated && e.Produto == produto {
			if err := l.runner.ApplyToTenant(ctx, e.TenantID); err != nil {
				// Stop before acking so the event is retried next tick.
				return err
			}
			log.Printf("provisioning: tenant %s migrations aplicadas", e.TenantID)
		}
	}
	// Ack up to the highest id handled (non-margot events are simply skipped).
	return l.consumer.Ack(ctx, batch[len(batch)-1].ID)
}

func (l *Listener) applyByString(ctx context.Context, tenantID string) error {
	id, err := uuid.Parse(tenantID)
	if err != nil {
		return err
	}
	return l.runner.ApplyToTenant(ctx, id)
}
