-- Métrica passou de "conversa" (janela 24h) para "resposta" (saída da IA):
-- a janela de serviço não existe mais. Remove a coluna do schema de cada tenant.
ALTER TABLE conversations DROP COLUMN IF EXISTS window_started_at;
