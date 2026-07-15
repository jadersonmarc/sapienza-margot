# SPEC — sapienza-margot

Data plane **Margot Atendente**: atendimento por WhatsApp multi-tenant. Envolve
(não reescreve) o motor de conversa do `rag-agente-go` e o pluga na plataforma
Sapienza (control plane `sapienza-core` + módulo `sapienza-kit`).

## Topologia

| Repo / módulo | Papel | Dono de qual dado | Deploy |
|---|---|---|---|
| `sapienza-core` | Control plane + console | schema `public` | serviço Coolify |
| `sapienza-margot` (este) | Data plane WhatsApp (Go) | schema `margot` (global) + suas tabelas em cada `tenant_<id>` | serviço Coolify |
| `sapienza-motor` | Data plane conteúdo (TS) | suas tabelas no mesmo `tenant_<id>` | serviço Coolify |
| `sapienza-kit` | Go module | tenancy, gating, eventos, authclient | importado por este |

## Regras de ouro

1. Um Postgres. `public` = control plane; `tenant_<id>` = dados do produto.
2. Margot **só LÊ `public`** (gating, product_rules) via kit; a única escrita
   sancionada em `public` é **append no `event_outbox`** (reportar uso/eventos).
3. Margot é dono só das **suas** tabelas: schema `margot` (product-global, roteamento
   + config + segredos) e as tabelas de conversa nos `tenant_<id>`. Roda suas próprias
   migrations de tenant.
4. Todo acesso a dado de conversa passa por `kit/tenancy.WithTenant`. Isolamento entre
   tenants = critério de aceite (vazamento zero).
5. Preço/regra vêm de `pricing.yaml` no core → lidos via `public` (`plans`,
   `product_rules`); nunca chumbados.

## WhatsApp — Evolution API

Reaproveita as integrações prontas do `rag-agente-go` (`internal/whatsapp/*`),
adaptadas ao multi-tenant. **Não** usa Meta Cloud API (decisão do dono do produto).
- Roteamento: `evolution_instance` (UNIQUE em `margot.tenant_channels`) → tenant.
- Envs globais da Sapienza: `EVOLUTION_API_URL`, `EVOLUTION_API_KEY`,
  `EVOLUTION_WEBHOOK_SECRET`.

## Modelo de dados

- **`margot` (schema product-global)**: `tenant_channels` (`tenant_id` PK,
  `evolution_instance` UNIQUE, `whatsapp_number`, `api_url?`, `api_key_enc?`,
  `system_prompt`, `tone`, `fallback`, `max_tokens`, `ai_model`, `business_hours`).
- **`tenant_<id>` (por tenant, via migrations de tenant)**: `contacts`,
  `pipeline_stages`, `conversations` (+ `window_started_at`), `messages`, `handoffs`,
  `knowledge_base`, `automations`.

## Ciclo de vida

- Escuta o outbox (`kit/events.Consumer{name:"margot"}`): em
  `SubscriptionActivated{margot}` (+ catch-up no boot) → aplica migrations de tenant
  via `kit/tenancy.MigrationRunner`.
- Inbound (webhook Evolution) → resolve tenant por instância → pipeline sob `WithTenant`.

## Medição & regras

- **Faturável = "conversa"**: abre uma janela de 24h (nova conversa ou `now -
  window_started_at > 24h`) → `UsageRecorded{metric:"conversa",count:1}` no outbox.
- **Handoff**: `count(messages) > handoff_max_mensagens` (de `product_rules`, default 15)
  → grava `handoffs`, emite `HandoffTriggered`, `mode='human'`, para de auto-responder.
- **Gating**: só auto-responde se a assinatura margot do tenant estiver ativa
  (`kit/gating.TenantCanOperate`).

## API do produto (para o console BFF)

`/api/v1/` (conversations, messages, send, handoff, config, channel, setup, contacts,
knowledge_base). Auth = **JWT curto do core** (`kit/authclient`); toda query escopada
ao tenant do JWT via `WithTenant`.

## Segredos

Credenciais por-tenant (ex.: API key de instância dedicada) cifradas **AES-256-GCM**
em repouso (`internal/secrets`, formato `iv:tag:ciphertext`, compatível com o padrão
do `spa-sapienza/lib/agent/crypto.ts`). Chave via `MARGOT_ENC_KEY`.

## Aceite

Ver o plano PROMPT B. Resumo: isolamento (vazamento zero), medição de conversa (janela
24h) refletida em `usage_counters` via trigger do core, handoff 15, auth de webhook,
provisioning aplica migrations, gating/JWT na API. Sobe independente no Coolify.
