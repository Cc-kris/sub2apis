-- REQ-015: quota monitoring supports all account platforms in the frozen matrix.
ALTER TABLE channel_monitors
  DROP CONSTRAINT IF EXISTS channel_monitors_provider_check;

ALTER TABLE channel_monitors
  ADD CONSTRAINT channel_monitors_provider_check
  CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok', 'antigravity', 'kimi', 'zhipu', 'deepseek'));
