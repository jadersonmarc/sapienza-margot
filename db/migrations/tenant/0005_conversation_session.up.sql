-- Handoff por SESSÃO (não acumulado) + sinalização de "precisa de humano".
-- bot_since: início da sessão atual do bot na conversa. Reseta quando a conversa
--   volta para o bot (devolver ao bot). O handoff por volume conta só as mensagens
--   desde este instante — antes contava o total acumulado e disparava "sem motivo".
-- needs_attention/attention_reason: destaque no console (e alerta) quando o bot
--   escala para humano ou marca um agendamento.
ALTER TABLE conversations
    ADD COLUMN IF NOT EXISTS bot_since        timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN IF NOT EXISTS needs_attention  boolean     NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS attention_reason text        NOT NULL DEFAULT '';
