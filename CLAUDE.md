# CLAUDE.md — sapienza-margot

## O que é

Data plane do produto **Margot** (atendimento WhatsApp) da plataforma Sapienza.
Envolve o motor de conversa do `../rag-agente-go` e o pluga no multi-tenant via
`../sapienza-kit`. Ver `SPEC.md` (arquitetura/regras) e `AGENTS.md` (convenções).

## Stack

- Go 1.26. Importa `github.com/jadersonmarc/sapienza-kit` (replace local `../sapienza-kit`).
- `pgx/v5` (à mão, sem sqlc). `anthropic-sdk-go` (Claude). `google/uuid`.
- Transporte WhatsApp: **Evolution API** (não Meta).

## Comandos

```bash
go build ./...
go test -p 1 ./...     # exige TEST_DATABASE_URL (integração compartilha 1 Postgres)
go vet ./...
```

## Estrutura (alvo)

- `cmd/server/main.go` — migra `margot`, conecta pool, sobe consumer de provisioning,
  mux `/health`, `/webhook/evolution`, `/api/v1/`.
- `db/migrations/margot/` — schema product-global (`tenant_channels`).
- `db/migrations/tenant/` — tabelas por tenant (aplicadas via `kit/tenancy.MigrationRunner`).
- `internal/secrets` — AES-256-GCM por tenant.
- `internal/channel` — resolver `ByInstance` (cache TTL 60s) sobre `margot.tenant_channels`.
- `internal/whatsapp` — webhook + client Evolution (+ MockSender) — de `rag-agente-go`.
- `internal/claude`, `internal/agent`, `internal/automation` — copiados de `rag-agente-go`.
- `internal/pipeline` — conversa sob `WithTenant`: CRM, billing (conversa), handoff, KB.
- `internal/store` — pgx à mão, tabelas de tenant (sem coluna tenant_id).
- `internal/api` — API do produto (JWT do core + gating).

## Convenções

- **`WithTenant` sempre em transação** — todo acesso a dado de conversa entra por ele.
- **Não escrever em `public`** exceto append no `event_outbox` (via `kit/events`).
- **Preço/regra nunca chumbados** — ler `plans`/`product_rules` (read-only).
- **pgx à mão** (SQL cru, revisável). Erros com `%w`.
- Segredos nunca em claro no repo; testes usam mocks.

## Restrições

- Não editar `../rag-agente-go`, `../spa-sapienza`, `../sapienza-core` fora do combinado.
- Não criar tabelas no `public`. Não usar Meta Cloud API (decisão: Evolution).
