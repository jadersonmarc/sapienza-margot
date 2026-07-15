# AGENTS.md — sapienza-margot

Convenções para agentes/dev no data plane Margot.

## Layout

```
sapienza-margot/
  go.mod                    module github.com/jadersonmarc/sapienza-margot (replace kit local)
  cmd/server/main.go        wiring: migrate margot, consumer de provisioning, mux
  db/migrations/margot/     schema product-global (tenant_channels) — aplicado no boot
  db/migrations/tenant/     tabelas por tenant (via kit MigrationRunner)
  internal/secrets/         AES-256-GCM (iv:tag:ciphertext)
  internal/channel/         resolver ByInstance + cache TTL
  internal/whatsapp/        WhatsAppDriver (Registry) + webhook + client Evolution + Meta stub + MockSender
  internal/claude/          adapter Claude (de rag-agente-go)
  internal/agent/           Replier seam (de rag-agente-go)
  internal/automation/      regras off-hours/welcome/keyword (de rag-agente-go)
  internal/pipeline/        conversa sob WithTenant (billing, handoff, KB)
  internal/store/           pgx à mão, tabelas de tenant
  internal/api/             API do produto (JWT do core + gating)
  internal/testutil/        helpers de teste (pool, provision, seed)
```

## Regras

- **Isolamento por schema**: nunca `tenant_id` em query de produto; sempre `WithTenant`
  numa transação. Teste de vazamento zero é aceite.
- **Escrita em public só via `kit/events`** (append no outbox). Leitura de `public`
  (gating/plans/product_rules) é read-only.
- **Driver por tenant**: `whatsapp.WhatsAppDriver` (`evolution` default, `meta` stub),
  escolhido por `margot.tenant_channels.driver`. Roteamento por `evolution_instance`
  (UNIQUE em `margot.tenant_channels`). Número **dedicado** é requisito de onboarding
  (`dedicated_number_confirmed`).
- **Billing "resposta"**: cada resposta **gerada pela IA e enviada** emite um
  `UsageRecorded{metric:"resposta"}` (na tx do outbound). Entrada, automações canned e
  envio manual do humano **não** faturam. Sem janela/sessão.
- **Handoff**: `handoff_max_mensagens` lido de `public.product_rules` (default 15).
- **Segredos**: `internal/secrets` cifra em repouso; chave `MARGOT_ENC_KEY`. Formato
  compatível com `spa-sapienza/lib/agent/crypto.ts`.
- **Eventos**: structs/contrato vêm de `kit/events` (não redefinir).

## Testes

- Integração exige `TEST_DATABASE_URL`; sem ela, `t.Skip`. Rodar `go test -p 1 ./...`.
- Provisionar 2 tenants, aplicar migrations de tenant, simular webhook `messages.upsert`
  (com `apikey`) e checar isolamento + billing + handoff. `MockSender` captura outbound;
  `Replier` stub no lugar do Claude real.
