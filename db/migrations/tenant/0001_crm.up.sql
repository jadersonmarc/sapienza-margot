-- Tabelas de conversa do Margot, aplicadas em CADA schema tenant_<id> via
-- kit/tenancy.MigrationRunner (rodam sob search_path do tenant). SEM coluna
-- tenant_id: o isolamento é o próprio schema.

CREATE TABLE pipeline_stages (
    id       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name     text NOT NULL,
    position int  NOT NULL DEFAULT 0,
    UNIQUE (name)
);

CREATE TABLE contacts (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    phone      text NOT NULL,
    name       text,
    source     text NOT NULL DEFAULT 'whatsapp',
    stage_id   uuid REFERENCES pipeline_stages(id) ON DELETE SET NULL,
    consent    boolean NOT NULL DEFAULT false,
    consent_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (phone)
);

CREATE TABLE conversations (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contact_id        uuid NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
    mode              text NOT NULL DEFAULT 'bot',   -- 'bot' | 'human' (handoff)
    status            text NOT NULL DEFAULT 'open',  -- 'open' | 'closed'
    -- Início da janela de serviço de 24h (unidade faturável "conversa").
    window_started_at timestamptz,
    last_message_at   timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (contact_id)
);

CREATE TABLE messages (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    direction       text NOT NULL,                 -- 'in' | 'out'
    sender          text NOT NULL,                 -- 'contact' | 'bot' | 'human'
    content         text NOT NULL,
    provider_id     text,
    status          text NOT NULL DEFAULT 'sent',
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_messages_conv_created ON messages (conversation_id, created_at);

CREATE TABLE handoffs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    reason          text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE knowledge_base (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    title      text NOT NULL,
    content    text NOT NULL,
    tags       text[] NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE automations (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    type       text NOT NULL,                  -- 'off_hours' | 'welcome' | 'keyword'
    trigger    jsonb NOT NULL DEFAULT '{}',
    action     jsonb NOT NULL DEFAULT '{}',
    enabled    boolean NOT NULL DEFAULT true,
    position   int NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_automations_enabled_pos ON automations (enabled, position);
