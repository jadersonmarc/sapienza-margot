-- Schema product-global do Margot: roteamento + config + segredos por tenant.
-- Cross-tenant (o webhook chega com o nome da instância, sem contexto de tenant),
-- por isso fica fora dos schemas tenant_<id>. Só o Margot escreve aqui.

CREATE SCHEMA IF NOT EXISTS margot;

CREATE TABLE IF NOT EXISTS margot.tenant_channels (
    tenant_id          uuid PRIMARY KEY,
    evolution_instance text NOT NULL UNIQUE,
    whatsapp_number    text,
    -- Override opcional (instância dedicada por tenant); senão usa envs globais.
    api_url            text,
    api_key_enc        text,          -- AES-256-GCM (iv:tag:ciphertext)
    system_prompt      text NOT NULL DEFAULT '',
    tone               text NOT NULL DEFAULT 'profissional',
    fallback           text NOT NULL DEFAULT '',
    max_tokens         int  NOT NULL DEFAULT 400,
    ai_model           text NOT NULL DEFAULT 'claude-haiku-4-5',
    business_hours     jsonb,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);
