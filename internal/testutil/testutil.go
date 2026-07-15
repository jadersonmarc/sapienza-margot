// Package testutil stands up the minimal control-plane schema Margot depends on
// (a subset of what sapienza-core owns, plus the usage-aggregation trigger) and
// provisions tenant schemas, so Margot can be tested end-to-end against one
// Postgres. Integration tests skip when TEST_DATABASE_URL is unset.
package testutil

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/tenancy"

	"github.com/jadersonmarc/sapienza-margot/db/migrations"
	"github.com/jadersonmarc/sapienza-margot/internal/db"
)

// Pool connects to TEST_DATABASE_URL or skips the test.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := envOrSkip(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect test db: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// SetupControlPlane creates the public subset + bus + margot schema and clears
// prior state, mirroring the column shapes and the UsageRecorded aggregation
// trigger that sapienza-core owns.
func SetupControlPlane(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		`CREATE SCHEMA IF NOT EXISTS bus`,
		`CREATE TABLE IF NOT EXISTS public.subscriptions (
			tenant_id uuid NOT NULL, produto text NOT NULL, tier text NOT NULL,
			status text NOT NULL DEFAULT 'active', hard_cap boolean NOT NULL DEFAULT false,
			activated_at timestamptz NOT NULL DEFAULT now(),
			PRIMARY KEY (tenant_id, produto))`,
		`CREATE TABLE IF NOT EXISTS public.product_rules (
			produto text PRIMARY KEY, rules jsonb NOT NULL DEFAULT '{}')`,
		`CREATE TABLE IF NOT EXISTS public.usage_counters (
			tenant_id uuid NOT NULL, produto text NOT NULL, period text NOT NULL,
			metric text NOT NULL, count integer NOT NULL DEFAULT 0,
			updated_at timestamptz NOT NULL DEFAULT now(),
			PRIMARY KEY (tenant_id, produto, period, metric))`,
		`CREATE TABLE IF NOT EXISTS public.event_outbox (
			id bigserial PRIMARY KEY, type text NOT NULL, tenant_id uuid NOT NULL,
			produto text, payload jsonb NOT NULL DEFAULT '{}',
			created_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS bus.event_cursors (
			consumer text PRIMARY KEY, last_id bigint NOT NULL DEFAULT 0,
			updated_at timestamptz NOT NULL DEFAULT now())`,
		// UsageRecorded -> usage_counters aggregation (same as core migration 0001).
		`CREATE OR REPLACE FUNCTION aggregate_usage_recorded() RETURNS trigger AS $$
		 DECLARE v_metric text; v_period text; v_count int;
		 BEGIN
		   IF NEW.type <> 'UsageRecorded' THEN RETURN NEW; END IF;
		   v_metric := NEW.payload->>'metric'; v_period := NEW.payload->>'period';
		   v_count := COALESCE((NEW.payload->>'count')::int, 0);
		   IF NEW.produto IS NULL OR v_metric IS NULL OR v_period IS NULL THEN RETURN NEW; END IF;
		   INSERT INTO public.usage_counters (tenant_id, produto, period, metric, count)
		   VALUES (NEW.tenant_id, NEW.produto, v_period, v_metric, v_count)
		   ON CONFLICT (tenant_id, produto, period, metric)
		   DO UPDATE SET count = public.usage_counters.count + EXCLUDED.count, updated_at = now();
		   RETURN NEW;
		 END; $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS event_outbox_aggregate_usage ON public.event_outbox`,
		`CREATE TRIGGER event_outbox_aggregate_usage AFTER INSERT ON public.event_outbox
		 FOR EACH ROW EXECUTE FUNCTION aggregate_usage_recorded()`,
		`TRUNCATE public.subscriptions, public.product_rules, public.usage_counters,
		          public.event_outbox, bus.event_cursors`,
		`INSERT INTO public.product_rules (produto, rules)
		 VALUES ('margot', '{"handoff_max_mensagens": 15}')
		 ON CONFLICT (produto) DO UPDATE SET rules = EXCLUDED.rules`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("setup control plane: %v\nstmt: %s", err, s)
		}
	}
	if err := db.MigrateMargot(ctx, pool, migrations.MargotSchema); err != nil {
		t.Fatalf("migrate margot: %v", err)
	}
	dropTenants(t, pool)
	t.Cleanup(func() { dropTenants(t, pool) })
	if _, err := pool.Exec(ctx, `TRUNCATE margot.tenant_channels`); err != nil {
		t.Fatalf("truncate channels: %v", err)
	}
}

// ProvisionTenant creates tenant_<id> and applies Margot's tenant migrations,
// plus an active subscription and a channel row (evolution_instance).
func ProvisionTenant(t *testing.T, pool *pgxpool.Pool, instance string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	tid := uuid.New()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := (tenancy.Provisioner{}).CreateSchema(ctx, tx, tid); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tenancy.NewMigrationRunner(pool, migrations.Tenant).ApplyToTenant(ctx, tid); err != nil {
		t.Fatalf("apply tenant migrations: %v", err)
	}
	SeedSubscription(t, pool, tid, "pro", "active")
	SeedChannel(t, pool, tid, instance)
	return tid
}

// SeedSubscription inserts/updates a subscription row.
func SeedSubscription(t *testing.T, pool *pgxpool.Pool, tid uuid.UUID, tier, status string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO public.subscriptions (tenant_id, produto, tier, status)
		 VALUES ($1, 'margot', $2, $3)
		 ON CONFLICT (tenant_id, produto) DO UPDATE SET tier = EXCLUDED.tier, status = EXCLUDED.status`,
		tid, tier, status)
	if err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
}

// SeedChannel inserts a tenant_channels row for the instance.
func SeedChannel(t *testing.T, pool *pgxpool.Pool, tid uuid.UUID, instance string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO margot.tenant_channels (tenant_id, evolution_instance, whatsapp_number, system_prompt, fallback)
		 VALUES ($1, $2, '5521999999999', 'Você é uma atendente.', 'Um momento, por favor.')
		 ON CONFLICT (tenant_id) DO UPDATE SET evolution_instance = EXCLUDED.evolution_instance`,
		tid, instance)
	if err != nil {
		t.Fatalf("seed channel: %v", err)
	}
}

func dropTenants(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `SELECT nspname FROM pg_namespace WHERE nspname LIKE 'tenant\_%' ESCAPE '\'`)
	if err != nil {
		t.Fatalf("list tenant schemas: %v", err)
	}
	var names []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	for _, n := range names {
		if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS "`+n+`" CASCADE`); err != nil {
			t.Fatalf("drop schema %s: %v", n, err)
		}
	}
}

func envOrSkip(t *testing.T) string {
	t.Helper()
	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
		return dsn
	}
	t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	return ""
}
