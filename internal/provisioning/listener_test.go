package provisioning_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-kit/tenancy"

	"github.com/jadersonmarc/sapienza-margot/internal/provisioning"
	"github.com/jadersonmarc/sapienza-margot/internal/testutil"
)

// TestCatchUpAppliesTenantMigrations: a tenant with an active margot
// subscription and an (empty) provisioned schema gets its tenant tables applied.
func TestCatchUpAppliesTenantMigrations(t *testing.T) {
	pool := testutil.Pool(t)
	testutil.SetupControlPlane(t, pool)
	ctx := context.Background()

	tid := uuid.New()
	// Core would create the empty schema on SubscriptionActivated; simulate that.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := (tenancy.Provisioner{}).CreateSchema(ctx, tx, tid); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	testutil.SeedSubscription(t, pool, tid, "pro", "active")

	// Before catch-up, the tenant tables do not exist yet.
	schema := pgx.Identifier{tenancy.SchemaName(tid)}.Sanitize()
	if tableExists(t, pool, schema, "contacts") {
		t.Fatal("contacts should not exist before provisioning")
	}

	if err := provisioning.New(pool).CatchUp(ctx); err != nil {
		t.Fatalf("catch-up: %v", err)
	}

	// After catch-up, the migrations were applied.
	if !tableExists(t, pool, schema, "contacts") {
		t.Fatal("contacts table should exist after provisioning")
	}
	if !tableExists(t, pool, schema, "conversations") {
		t.Fatal("conversations table should exist after provisioning")
	}
}

func tableExists(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, schemaQuoted, table string) bool {
	t.Helper()
	var n int
	// information_schema compares against the raw (unquoted) schema name.
	name := schemaQuoted
	if len(name) >= 2 && name[0] == '"' {
		name = name[1 : len(name)-1]
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_schema=$1 AND table_name=$2`,
		name, table).Scan(&n); err != nil {
		t.Fatalf("table exists check: %v", err)
	}
	return n > 0
}
