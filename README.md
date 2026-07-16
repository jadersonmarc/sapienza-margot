# sapienza-margot

Data plane do produto **Margot Atendente**: atendimento por WhatsApp, multi-tenant. Envolve
(não reescreve) o motor de conversa do `../rag-agente-go` e o pluga na plataforma Sapienza.

Sobe **independente** do core no Coolify, contra o mesmo Postgres — deploy de um não derruba o
outro.

## Como se encaixa

| Repo | Papel | Dono de qual dado |
|---|---|---|
| `sapienza-core` | Control plane + console | schema `public` |
| **`sapienza-margot`** (este) | Data plane WhatsApp (Go) | schema `margot` (global) + suas tabelas em cada `tenant_<id>` |
| `sapienza-motor` | Data plane conteúdo (TS) | suas tabelas nos mesmos `tenant_<id>` |
| `sapienza-kit` | Módulo Go: tenancy, gating, eventos, authclient | — (importado por este) |

## Stack

Go 1.26 · `pgx/v5` (SQL cru, sem sqlc) · `anthropic-sdk-go` (Claude) · importa
`sapienza-kit` (replace local `../sapienza-kit`, vendorizado).

## Arquitetura

```
cmd/server/main.go        migra `margot`, monta drivers/pipeline/API, sobe o consumer de
                          provisioning e o mux: /health, /webhook/evolution, /api/v1/
db/migrations/margot/     schema product-global: tenant_channels (roteamento + config)
db/migrations/tenant/     tabelas por tenant, aplicadas via kit/tenancy.MigrationRunner
internal/channel          resolve instância → tenant (cache TTL 60s)
internal/whatsapp         WhatsAppDriver + Registry + webhook + client Evolution + Meta (stub)
internal/pipeline         a conversa: CRM, automações, IA, billing, handoff — sob WithTenant
internal/store            pgx à mão; tabelas de tenant, sem coluna tenant_id
internal/claude           Replier sobre a Messages API
internal/api              API do produto (JWT do core + gating)
internal/secrets          AES-256-GCM por tenant (formato iv:tag:ciphertext, interop com o TS)
```

### Modelo de dados

- **schema `margot`** (product-global): `tenant_channels` — `tenant_id` PK,
  `evolution_instance` UNIQUE (é o que roteia o webhook → tenant), `whatsapp_number`, `driver`,
  `dedicated_number_confirmed`, `system_prompt`, `tone`, `fallback`, `max_tokens`, `ai_model`,
  `business_hours`. Fica fora dos schemas de tenant porque o webhook chega só com o nome da
  instância, sem contexto de tenant.
- **`tenant_<id>`** (por tenant): `contacts`, `pipeline_stages`, `conversations`, `messages`,
  `handoffs`, `knowledge_base`, `automations`.

### O caminho de uma mensagem

```
webhook Evolution → autoriza (apikey) → filtra messages.upsert → resolve tenant pela instância
  → persiste inbound (nunca fatura)
  → mode != bot? para aqui
  → gating: assinatura margot ativa? senão, silêncio
  → count(messages) > handoff_max_mensagens (default 15)? → handoff, mode=human, para de responder
  → automação casou? → envia resposta enlatada (NÃO fatura)
  → IA gera resposta (fora de qualquer transação) → envia → UsageRecorded na mesma tx do outbound
```

**Faturável = resposta da IA.** A entrada do cliente é grátis; automações e o envio manual do
humano pelo console também não faturam. Excedente R$ 0,50/resposta; tiers 500/1.500/5.000.
Sem janela de 24h e sem máquina de sessão — isso é da Meta, não do Evolution.

### Driver de WhatsApp

O pipeline **nunca** fala com um provedor: fala com `whatsapp.WhatsAppDriver`. O driver é
escolhido **por tenant** (`margot.tenant_channels.driver`), então trocar não toca o pipeline.

- **`evolution`** (default) — WhatsApp não-oficial (Baileys). Sem janela de 24h, sem categorias,
  sem cobrança por mensagem da Meta.
- **`meta`** — Cloud API oficial. **Stub**: selecionável, mas `SendText` retorna erro. Quando
  for implementado, voltam janela de serviço, templates e **custo por mensagem**, que precisa
  entrar na precificação daquele tenant.

**Número dedicado** é requisito de onboarding: o número conectado via Evolution nunca deve ser o
pessoal do responsável. A confirmação fica em `dedicated_number_confirmed` e é exposta no
`GET /setup`.

## Regras de ouro

1. **`WithTenant` sempre, dentro de transação** — todo acesso a dado de conversa entra por ele.
   Isolamento entre tenants é critério de aceite (`internal/pipeline/pipeline_test.go`).
2. **Não escrever em `public`** — exceto o append no `event_outbox` via `kit/events`.
3. **Preço/regra nunca chumbados** — ler `plans`/`product_rules` (read-only).
4. **WhatsApp sempre via `WhatsAppDriver`** — nunca falar com Evolution/Meta direto no pipeline.
5. **pgx à mão**: SQL cru e revisável. Erros com `%w`.

## Desenvolvimento

```bash
go build ./...
go vet ./...
go test -p 1 ./...   # -p 1: os pacotes compartilham 1 Postgres e truncam entre si
```

> **Os testes de integração pulam sem `TEST_DATABASE_URL`.** Sem ela, `go test ./...` passa
> verde cobrindo só `secrets` e o webhook — isolamento, billing, handoff e a API **não são
> exercitados**. Para rodar a suíte inteira:
> ```bash
> docker run -d --name pg-test -e POSTGRES_PASSWORD=postgres -p 55432:5432 postgres:16
> TEST_DATABASE_URL=postgres://postgres:postgres@localhost:55432/postgres go test -p 1 ./...
> ```
> O harness **trunca o control plane e dropa os schemas `tenant_*`** — nunca aponte para um
> banco real.

> **O kit é vendorizado** (`vendor/`, incluindo `sapienza-kit`), para o build Docker ser
> hermético. Com `vendor/` presente o Go compila **do vendor**, não do `replace`: mudou o kit,
> rode `go mod vendor` — senão a CI passa verde no código antigo.

## Variáveis de ambiente

| Var | Obrigatória | Observação |
|---|:-:|---|
| `DATABASE_URL` | ✅ | o MESMO Postgres do core |
| `PRODUCT_JWT_SECRET` | ✅ | **o MESMO valor do core**, que emite o JWT; aqui só validamos |
| `PORT` | — | default 8081 |
| `MARGOT_ENC_KEY` | recomendada | AES-256-GCM (32 bytes base64); sem ela os segredos por tenant não são decifrados |
| `EVOLUTION_API_URL` / `EVOLUTION_API_KEY` | ✅ p/ enviar | servidor Evolution da Sapienza |
| `EVOLUTION_WEBHOOK_SECRET` | ✅ | autentica o webhook; **obrigatória em produção** |
| `ANTHROPIC_API_KEY` | — | sem ela o bot responde só o `fallback` do tenant |

## Deploy

Ver **[`../sapienza-core/DEPLOY.md`](../sapienza-core/DEPLOY.md)**. O boot migra o schema
`margot`, faz catch-up de provisioning e sobe a API — idempotente, sem passo manual.

Webhook a configurar na Evolution: `POST https://<seu-margot>/webhook/evolution`, header
`apikey: <EVOLUTION_WEBHOOK_SECRET>`.

## Estado atual

O núcleo — isolamento entre tenants, billing por resposta, handoff, seam de driver e auth por
JWT — está coberto por testes de aceitação que checam vazamento de verdade.

Em evolução: o driver `meta` é um stub (só `evolution` envia hoje); a busca na base de
conhecimento é por `ILIKE` no título (pgvector é fast-follow).

Ver também `SPEC.md`, `CLAUDE.md`, `AGENTS.md` e `../INVENTORY.md`.
