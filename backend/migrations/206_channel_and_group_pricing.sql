-- REQ-004/009: optional time/service-tier channel pricing and group model pricing.
ALTER TABLE channel_model_pricing ADD COLUMN IF NOT EXISTS time_pricing JSONB;
ALTER TABLE channel_model_pricing ADD COLUMN IF NOT EXISTS fast_multiplier NUMERIC(20,10);
ALTER TABLE channel_model_pricing ADD COLUMN IF NOT EXISTS flex_multiplier NUMERIC(20,10);
ALTER TABLE groups ADD COLUMN IF NOT EXISTS long_context_pricing_enabled BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE groups ADD COLUMN IF NOT EXISTS model_pricing JSONB NOT NULL DEFAULT '{}'::jsonb;
