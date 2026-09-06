-- TASK-028/029 foundation: classify monitors that use active probes, passive
-- telemetry, or quota snapshots. Existing monitors keep the active behavior.
ALTER TABLE channel_monitors
  ADD COLUMN IF NOT EXISTS mode VARCHAR(16) NOT NULL DEFAULT 'active';

ALTER TABLE channel_monitors
  DROP CONSTRAINT IF EXISTS channel_monitors_mode_check;

ALTER TABLE channel_monitors
  ADD CONSTRAINT channel_monitors_mode_check
  CHECK (mode IN ('active', 'passive', 'quota'));
