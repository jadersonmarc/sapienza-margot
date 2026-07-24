-- Estágios padrão do funil, semeados por tenant (o schema é a fronteira; sem
-- tenant_id). Espelha o seed do rag-agente-go. Idempotente: só semeia se o funil
-- ainda estiver vazio, para não colidir com estágios já criados à mão.
INSERT INTO pipeline_stages (name, position)
SELECT v.name, v.position
  FROM (VALUES
    ('Novo lead', 0),
    ('Em conversa', 1),
    ('Diagnóstico agendado', 2),
    ('Proposta', 3),
    ('Cliente', 4)
  ) AS v(name, position)
 WHERE NOT EXISTS (SELECT 1 FROM pipeline_stages);
