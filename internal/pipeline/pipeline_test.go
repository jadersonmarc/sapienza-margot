package pipeline_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

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

// countingReplier conta as chamadas ao modelo — cada uma é custo real, então os
// testes de idempotência e de cap asseram sobre este número, não só sobre o envio.
type countingReplier struct {
	mu    sync.Mutex
	calls int
	reply string
}

func (c *countingReplier) Reply(_ context.Context, _, _ string, _ []agent.Turn, _ int) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.reply, nil
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

func usageResposta(t *testing.T, pool *pgxpool.Pool, tid uuid.UUID) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(count,0) FROM public.usage_counters
		 WHERE tenant_id=$1 AND produto='margot' AND metric='resposta'`, tid).Scan(&n)
	if err == pgx.ErrNoRows {
		return 0
	}
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	return n
}

// registry wraps the mock as the evolution driver for the pipeline.
func registry(mock *whatsapp.MockSender) *whatsapp.Registry {
	return whatsapp.NewRegistry("evolution", mock)
}

func TestTenantIsolation(t *testing.T) {
	pool := testutil.Pool(t)
	testutil.SetupControlPlane(t, pool)

	a := testutil.ProvisionTenant(t, pool, "inst-a")
	b := testutil.ProvisionTenant(t, pool, "inst-b")
	chA := resolveChannel(t, pool, "inst-a")
	chB := resolveChannel(t, pool, "inst-b")

	mock := &whatsapp.MockSender{}
	p := pipeline.New(pool, registry(mock), stubReplier{"olá"}, gating.New(pool))
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

func TestBillingResposta(t *testing.T) {
	pool := testutil.Pool(t)
	testutil.SetupControlPlane(t, pool)
	a := testutil.ProvisionTenant(t, pool, "inst-a")
	ch := resolveChannel(t, pool, "inst-a")
	ctx := context.Background()

	p := pipeline.New(pool, registry(&whatsapp.MockSender{}), stubReplier{"ok"}, gating.New(pool))

	// Each inbound that yields an AI reply bills exactly one "resposta".
	if err := p.Process(ctx, ch, inbound("inst-a", "111", "oi")); err != nil {
		t.Fatal(err)
	}
	if got := usageResposta(t, pool, a); got != 1 {
		t.Fatalf("após 1 resposta da IA, resposta = %d, want 1", got)
	}
	if err := p.Process(ctx, ch, inbound("inst-a", "111", "tudo bem?")); err != nil {
		t.Fatal(err)
	}
	if got := usageResposta(t, pool, a); got != 2 {
		t.Fatalf("após 2 respostas da IA, resposta = %d, want 2", got)
	}

	// Inbound on a human-owned conversation generates no AI reply → not billed.
	schema := pgx.Identifier{tenancy.SchemaName(a)}.Sanitize()
	if _, err := pool.Exec(ctx, fmt.Sprintf(`UPDATE %s.conversations SET mode='human'`, schema)); err != nil {
		t.Fatal(err)
	}
	if err := p.Process(ctx, ch, inbound("inst-a", "111", "operador assume")); err != nil {
		t.Fatal(err)
	}
	if got := usageResposta(t, pool, a); got != 2 {
		t.Fatalf("entrada em conversa human não deve faturar, resposta = %d, want 2", got)
	}

	// Automation replies are canned (not AI-generated) → not billed, even though a
	// message is sent. A fresh tenant with a welcome automation: first inbound fires
	// the welcome, sends one outbound, bills zero.
	b := testutil.ProvisionTenant(t, pool, "inst-b")
	chB := resolveChannel(t, pool, "inst-b")
	schemaB := pgx.Identifier{tenancy.SchemaName(b)}.Sanitize()
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s.automations (type, action) VALUES ('welcome', '{"reply":"Bem-vindo!"}')`, schemaB)); err != nil {
		t.Fatal(err)
	}
	if err := p.Process(ctx, chB, inbound("inst-b", "222", "olá")); err != nil {
		t.Fatal(err)
	}
	if got := tenantCount(t, pool, b, "messages", "WHERE direction='out'"); got != 1 {
		t.Fatalf("automação deveria enviar 1 outbound, got %d", got)
	}
	if got := usageResposta(t, pool, b); got != 0 {
		t.Fatalf("automação não deve faturar resposta, got %d", got)
	}
}

// A Evolution reentrega a mensagem quando o webhook erra ou estoura o tempo. Sem
// dedup por provider_id isso reprocessava tudo: nova chamada ao modelo (que
// pagamos), resposta duplicada ao contato e outra "resposta" faturada.
func TestInboundIdempotente(t *testing.T) {
	pool := testutil.Pool(t)
	testutil.SetupControlPlane(t, pool)
	a := testutil.ProvisionTenant(t, pool, "inst-a")
	ch := resolveChannel(t, pool, "inst-a")
	ctx := context.Background()

	mock := &whatsapp.MockSender{}
	replier := &countingReplier{reply: "ok"}
	p := pipeline.New(pool, registry(mock), replier, gating.New(pool))

	// inbound() deriva ProviderID do texto: as duas entregas são a mesma mensagem.
	in := inbound("inst-a", "111", "oi")
	if err := p.Process(ctx, ch, in); err != nil {
		t.Fatal(err)
	}
	// Reentrega (retry da Evolution).
	if err := p.Process(ctx, ch, in); err != nil {
		t.Fatalf("reentrega não pode virar erro (500 faria a Evolution retentar de novo): %v", err)
	}

	if got := tenantCount(t, pool, a, "messages", "WHERE direction='in'"); got != 1 {
		t.Fatalf("a mesma mensagem foi gravada %d vezes, want 1", got)
	}
	if replier.calls != 1 {
		t.Fatalf("modelo chamado %d vezes para a mesma mensagem, want 1 (custo dobrado)", replier.calls)
	}
	if got := len(mock.Sent); got != 1 {
		t.Fatalf("contato recebeu %d respostas, want 1", got)
	}
	if got := usageResposta(t, pool, a); got != 1 {
		t.Fatalf("resposta faturada %d vezes, want 1 (cobrança em dobro)", got)
	}

	// Mensagem diferente do mesmo contato segue normalmente.
	if err := p.Process(ctx, ch, inbound("inst-a", "111", "outra pergunta")); err != nil {
		t.Fatal(err)
	}
	if got := usageResposta(t, pool, a); got != 2 {
		t.Fatalf("nova mensagem deve faturar, resposta = %d, want 2", got)
	}
}

// Com hard_cap, ao atingir o incluso o bot para de gerar: o inbound continua
// registrado (aparece no console), mas o modelo — que é o custo — não é chamado.
func TestHardCapNaoGeraResposta(t *testing.T) {
	pool := testutil.Pool(t)
	testutil.SetupControlPlane(t, pool)
	a := testutil.ProvisionTenant(t, pool, "inst-a")
	ch := resolveChannel(t, pool, "inst-a")
	ctx := context.Background()

	// Plano start: 500 respostas incluídas; tenant no cap rígido, já no limite.
	if _, err := pool.Exec(ctx,
		`INSERT INTO public.plans (produto, tier, mensal, incluso, metric, excedente_unitario)
		 VALUES ('margot','start',400,500,'resposta',0.50)
		 ON CONFLICT (produto, tier) DO UPDATE SET incluso = EXCLUDED.incluso`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE public.subscriptions SET tier='start', hard_cap=true WHERE tenant_id=$1 AND produto='margot'`, a); err != nil {
		t.Fatal(err)
	}
	period := time.Now().UTC().Format("2006-01")
	if _, err := pool.Exec(ctx,
		`INSERT INTO public.usage_counters (tenant_id, produto, period, metric, count)
		 VALUES ($1,'margot',$2,'resposta',500)`, a, period); err != nil {
		t.Fatal(err)
	}

	mock := &whatsapp.MockSender{}
	replier := &countingReplier{reply: "ok"}
	p := pipeline.New(pool, registry(mock), replier, gating.New(pool))

	if err := p.Process(ctx, ch, inbound("inst-a", "111", "oi")); err != nil {
		t.Fatal(err)
	}
	if replier.calls != 0 {
		t.Fatalf("no cap o modelo não pode ser chamado (é o custo), calls = %d", replier.calls)
	}
	if got := len(mock.Sent); got != 0 {
		t.Fatalf("no cap nada deve ser enviado, got %d", got)
	}
	if got := tenantCount(t, pool, a, "messages", "WHERE direction='in'"); got != 1 {
		t.Fatalf("o inbound deve continuar registrado, got %d", got)
	}
	if got := usageResposta(t, pool, a); got != 500 {
		t.Fatalf("nada novo pode ser faturado no cap, resposta = %d, want 500", got)
	}
}

func TestHandoffAfterMax(t *testing.T) {
	pool := testutil.Pool(t)
	testutil.SetupControlPlane(t, pool)
	a := testutil.ProvisionTenant(t, pool, "inst-a")
	ch := resolveChannel(t, pool, "inst-a")
	ctx := context.Background()

	mock := &whatsapp.MockSender{}
	p := pipeline.New(pool, registry(mock), stubReplier{"ok"}, gating.New(pool))

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
