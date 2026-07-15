# sapienza-margot

Data plane **Margot Atendente** — atendimento por WhatsApp (driver Evolution default,
Meta plugável), multi-tenant, plugado na plataforma Sapienza (control plane
`sapienza-core` + módulo `sapienza-kit`). Envolve o motor de conversa do
`../rag-agente-go`. Faturamento por **resposta da IA** (entrada é grátis).

## Stack

Go 1.26 · pgx/v5 · anthropic-sdk-go · WhatsApp (Evolution default / Meta plugável) ·
importa `sapienza-kit`.

## Desenvolvimento

```bash
go build ./...
export TEST_DATABASE_URL=postgres://...   # p/ testes de integração
go test -p 1 ./...
```

## Execução

Envs: `DATABASE_URL`, `MARGOT_ENC_KEY`, `PRODUCT_JWT_SECRET`, `EVOLUTION_API_URL`,
`EVOLUTION_API_KEY`, `EVOLUTION_WEBHOOK_SECRET`, `ANTHROPIC_API_KEY`.

Sobe independente do core no Coolify (mesmo Postgres). Ver `SPEC.md`, `CLAUDE.md`,
`AGENTS.md` e `../INVENTORY.md`. Faz parte do PROMPT B.
