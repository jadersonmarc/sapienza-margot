package pipeline_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/gating"
	"github.com/jadersonmarc/sapienza-kit/tenancy"

	"github.com/jadersonmarc/sapienza-margot/internal/agent"
	"github.com/jadersonmarc/sapienza-margot/internal/channel"
	"github.com/jadersonmarc/sapienza-margot/internal/pipeline"
	"github.com/jadersonmarc/sapienza-margot/internal/testutil"
	"github.com/jadersonmarc/sapienza-margot/internal/whatsapp"
)

type stubReplier struct{ reply string }

func (s stubReplier) Reply(_ context.Context, _, _ string, _ []agent.Turn, _ int) (string, error) {
	return s.reply, nil
}

func inbound(instance, phone, text string) whatsapp.Inbound {
	return whatsapp.Inbound{Instance: instance, Phone: phone, Text: text, ProviderID: "pid-" + text}
}

func resolveChannel(t *testing.T, pool *pgxpool.Pool, instance string) channel.TenantChannel {
	t.Helper()
	ch, err := channel.NewLoader(pool, nil).ByInstance(context.Background(), instance)
	if err != nil {
		t.Fatalf("resolve channel %q: %v", instance, err)
	}
	return ch
}

// tenantCount counts rows in a tenant schema table (asserts isolation directly).
func tenantCount(t *testing.T, pool *pgxpool.Pool, tid uuid.UUID, table, where string, args ...any) int {
	t.Helper()
	schema := pgx.Identifier{tenancy.SchemaName(tid)}.Sanitize()
	q := fmt.Sprintf("SELECT count(*) FROM %s.%s %s", schema, table, where)
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func usageConversa(t *testing.T, pool *pgxpool.Pool, tid uuid.UUID) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(count,0) FROM public.usage_counters
		 WHERE tenant_id=$1 AND produto='margot' AND metric='conversa'`, tid).Scan(&n)
	if err == pgx.ErrNoRows {
		return 0
	}
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	return n
}

func TestTenantIsolation(t *testing.T) {
	pool := testutil.Pool(t)
	testutil.SetupControlPlane(t, pool)

	a := testutil.ProvisionTenant(t, pool, "inst-a")
	b := testutil.ProvisionTenant(t, pool, "inst-b")
	chA := resolveChannel(t, pool, "inst-a")
	chB := resolveChannel(t, pool, "inst-b")

	mock := &whatsapp.MockSender{}
	p := pipeline.New(pool, mock, stubReplier{"olá"}, gating.New(pool))
	ctx := context.Background()

	if err := p.Process(ctx, chA, inbound("inst-a", "111", "oi A")); err != nil {
		t.Fatalf("process A: %v", err)
	}
	if err := p.Process(ctx, chB, inbound("inst-b", "222", "oi B")); err != nil {
		t.Fatalf("process B: %v", err)
	}

	// Each tenant sees only its own contact — zero leakage.
	if got := tenantCount(t, pool, a, "contacts", "WHERE phone=$1", "111"); got != 1 {
		t.Fatalf("tenant A contact 111 = %d, want 1", got)
	}
	if got := tenantCount(t, pool, a, "contacts", "WHERE phone=$1", "222"); got != 0 {
		t.Fatalf("LEAK: tenant A sees contact 222 (%d)", got)
	}
	if got := tenantCount(t, pool, b, "contacts", "WHERE phone=$1", "111"); got != 0 {
		t.Fatalf("LEAK: tenant B sees contact 111 (%d)", got)
	}
	// Bot replied once per tenant.
	if got := tenantCount(t, pool, a, "messages", "WHERE direction='out'"); got != 1 {
		t.Fatalf("tenant A outbound = %d, want 1", got)
	}
	if len(mock.Messages()) != 2 {
		t.Fatalf("mock sent %d, want 2", len(mock.Messages()))
	}
}

func TestBillingConversaWindow(t *testing.T) {
	pool := testutil.Pool(t)
	testutil.SetupControlPlane(t, pool)
	a := testutil.ProvisionTenant(t, pool, "inst-a")
	ch := resolveChannel(t, pool, "inst-a")
	ctx := context.Background()

	p := pipeline.New(pool, &whatsapp.MockSender{}, stubReplier{"ok"}, gating.New(pool))

	// First message opens a window → 1 conversa.
	if err := p.Process(ctx, ch, inbound("inst-a", "111", "oi")); err != nil {
		t.Fatal(err)
	}
	if got := usageConversa(t, pool, a); got != 1 {
		t.Fatalf("após 1ª msg, conversa = %d, want 1", got)
	}
	// Reply within the window → still 1 (no new billable conversa).
	if err := p.Process(ctx, ch, inbound("inst-a", "111", "tudo bem?")); err != nil {
		t.Fatal(err)
	}
	if got := usageConversa(t, pool, a); got != 1 {
		t.Fatalf("dentro da janela, conversa = %d, want 1", got)
	}
	// Age the window past 24h → next message opens a new conversa.
	schema := pgx.Identifier{tenancy.SchemaName(a)}.Sanitize()
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		`UPDATE %s.conversations SET window_started_at = now() - interval '25 hours'`, schema)); err != nil {
		t.Fatal(err)
	}
	if err := p.Process(ctx, ch, inbound("inst-a", "111", "voltei")); err != nil {
		t.Fatal(err)
	}
	if got := usageConversa(t, pool, a); got != 2 {
		t.Fatalf("após >24h, conversa = %d, want 2", got)
	}
}

func TestHandoffAfterMax(t *testing.T) {
	pool := testutil.Pool(t)
	testutil.SetupControlPlane(t, pool)
	a := testutil.ProvisionTenant(t, pool, "inst-a")
	ch := resolveChannel(t, pool, "inst-a")
	ctx := context.Background()

	mock := &whatsapp.MockSender{}
	p := pipeline.New(pool, mock, stubReplier{"ok"}, gating.New(pool))

	// handoff_max=15; each exchange adds ~2 messages, so handoff fires by ~9 inbounds.
	for i := 0; i < 12; i++ {
		if err := p.Process(ctx, ch, inbound("inst-a", "111", fmt.Sprintf("msg %d", i))); err != nil {
			t.Fatalf("process %d: %v", i, err)
		}
	}

	if got := tenantCount(t, pool, a, "handoffs", ""); got < 1 {
		t.Fatalf("esperava >=1 handoff, got %d", got)
	}
	if got := tenantCount(t, pool, a, "conversations", "WHERE mode='human'"); got != 1 {
		t.Fatalf("conversa deveria estar em modo human, got %d", got)
	}
	var evts int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM public.event_outbox WHERE tenant_id=$1 AND type='HandoffTriggered'`, a).Scan(&evts); err != nil {
		t.Fatal(err)
	}
	if evts < 1 {
		t.Fatalf("esperava evento HandoffTriggered, got %d", evts)
	}
}
