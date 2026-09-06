CREATE TABLE IF NOT EXISTS openai_team_block_events (
    id BIGSERIAL PRIMARY KEY,
    team_id TEXT NOT NULL,
    trigger_request_id TEXT NOT NULL,
    trigger_account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    state TEXT NOT NULL DEFAULT 'blocked' CHECK (state IN ('blocked', 'probe_due', 'probing', 'cleared')),
    block_until TIMESTAMPTZ NOT NULL,
    probe_owner TEXT,
    probe_until TIMESTAMPTZ,
    cleared_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (team_id, trigger_request_id)
);

CREATE INDEX IF NOT EXISTS openai_team_block_events_activity_idx
    ON openai_team_block_events (state, block_until)
    WHERE state <> 'cleared';

CREATE INDEX IF NOT EXISTS accounts_chatgpt_account_id_idx
    ON accounts ((credentials->>'chatgpt_account_id'))
    WHERE deleted_at IS NULL;
