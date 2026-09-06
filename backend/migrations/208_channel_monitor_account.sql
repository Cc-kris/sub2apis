-- TASK-028: optionally bind a monitor to one upstream account.
ALTER TABLE channel_monitors
  ADD COLUMN IF NOT EXISTS account_id BIGINT;

ALTER TABLE channel_monitors
  DROP CONSTRAINT IF EXISTS channel_monitors_account_id_fkey;

ALTER TABLE channel_monitors
  ADD CONSTRAINT channel_monitors_account_id_fkey
  FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_channel_monitors_account_id
  ON channel_monitors (account_id);
