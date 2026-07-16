-- Idempotência do inbound: a Evolution retenta quando o webhook devolve erro ou
-- estoura o tempo, e sem uma chave natural o retry reprocessa a mensagem — nova
-- chamada ao modelo, nova resposta ao contato e nova "resposta" faturada. O
-- provider_id (id da mensagem na Evolution) é essa chave.
--
-- Índice PARCIAL: mensagens sem id do provedor gravam NULL (pipeline.optional) e
-- não devem colidir entre si. Em unique padrão NULLs já não colidem, mas o
-- predicado deixa a intenção explícita em vez de acidental.
CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_provider_id
    ON messages (provider_id) WHERE provider_id IS NOT NULL;
