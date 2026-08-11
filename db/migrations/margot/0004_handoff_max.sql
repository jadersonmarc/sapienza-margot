-- Limite de handoff por TENANT (antes vinha só do product_rules global = 15).
-- Nº de mensagens da sessão do bot antes de passar para humano. 0 = nunca passa
-- automaticamente (só handoff manual ou por automação). Editável no Agente.
ALTER TABLE margot.tenant_channels
    ADD COLUMN IF NOT EXISTS handoff_max int NOT NULL DEFAULT 15;
