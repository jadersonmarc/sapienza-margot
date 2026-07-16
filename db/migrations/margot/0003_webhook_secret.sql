-- Segredo de webhook POR TENANT. Com um único segredo global, quem o obtivesse
-- (log, backup, config da Evolution) forjaria um payload dizendo ser a instância
-- de qualquer cliente: mensagem injetada no schema dele e chamada ao modelo paga
-- por nós, com receita zero. O segredo passa a ser por instância, cifrado em
-- repouso (AES-256-GCM, iv:tag:ciphertext) como api_key_enc.
ALTER TABLE margot.tenant_channels
  ADD COLUMN IF NOT EXISTS webhook_secret_enc text;
