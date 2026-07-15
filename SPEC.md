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

## WhatsApp — driver abstrato (Evolution default, Meta plugável)

A Margot **nunca** fala com um provedor direto: fala com a interface
`whatsapp.WhatsAppDriver`. O driver é escolhido **por tenant**
(`margot.tenant_channels.driver`); trocar de driver não toca o pipeline.
- **`evolution`** — **default.** WhatsApp não-oficial (Baileys) via Evolution API;
  reaproveita as integrações do `rag-agente-go` (`internal/whatsapp/*`). Sem janela de
  24h, sem categorias, sem cobrança de Meta por mensagem.
- **`meta`** — Meta Cloud API oficial, **plugável (stub por ora)**. Ao ser implementado
  voltam janela de serviço, templates e **custo por mensagem da Meta** — a considerar na
  precificação daquele tenant.
- **Número dedicado (requisito de onboarding):** o número conectado via Evolution
  **deve ser dedicado**, nunca o número pessoal/principal do responsável. Confirmado em
  `dedicated_number_confirmed` antes de considerar o canal conectado.
- Roteamento: `evolution_instance` (UNIQUE em `margot.tenant_channels`) → tenant.
- Envs globais da Sapienza: `EVOLUTION_API_URL`, `EVOLUTION_API_KEY`,
  `EVOLUTION_WEBHOOK_SECRET`.

## Modelo de dados

- **`margot` (schema product-global)**: `tenant_channels` (`tenant_id` PK,
  `evolution_instance` UNIQUE, `whatsapp_number`, `driver` default `evolution`,
  `dedicated_number_confirmed`, `api_url?`, `api_key_enc?`, `system_prompt`, `tone`,
  `fallback`, `max_tokens`, `ai_model`, `business_hours`).
- **`tenant_<id>` (por tenant, via migrations de tenant)**: `contacts`,
  `pipeline_stages`, `conversations`, `messages`, `handoffs`, `knowledge_base`,
  `automations`.

## Ciclo de vida

- Escuta o outbox (`kit/events.Consumer{name:"margot"}`): em
  `SubscriptionActivated{margot}` (+ catch-up no boot) → aplica migrations de tenant
  via `kit/tenancy.MigrationRunner`.
- Inbound (webhook Evolution) → resolve tenant por instância → pipeline sob `WithTenant`.

## Medição & regras

- **Faturável = "resposta"**: cada mensagem **gerada pela IA e enviada** emite
  `UsageRecorded{metric:"resposta",count:1}` no outbox (na mesma tx do outbound).
  **Entrada do cliente é grátis** e nunca conta; **automações** (respostas canned) e
  **envio manual do humano** no console também **não** faturam. Excedente R$ 0,50/resposta;
  tiers 500/1.500/5.000. Sem janela de 24h / máquina de sessão.
- **Handoff**: `count(messages) > handoff_max_mensagens` (de `product_rules`, default 15)
  → grava `handoffs`, emite `HandoffTriggered`, `mode='human'`, para de auto-responder
  (a partir daí a IA não gera resposta, logo nada é faturado).
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

Ver o plano PROMPT B. Resumo: isolamento (vazamento zero), medição por **resposta**
(saída da IA) refletida em `usage_counters` via trigger do core, handoff 15, auth de
webhook, provisioning aplica migrations, gating/JWT na API, driver selecionável por
tenant. Sobe independente no Coolify.
