-- Driver de WhatsApp por tenant (Evolution default, Meta plugável) + confirmação
-- do número dedicado (requisito de onboarding antes de ativar o canal Evolution).
ALTER TABLE margot.tenant_channels
  ADD COLUMN IF NOT EXISTS driver text NOT NULL DEFAULT 'evolution';

ALTER TABLE margot.tenant_channels
  ADD COLUMN IF NOT EXISTS dedicated_number_confirmed boolean NOT NULL DEFAULT false;
