CREATE TABLE IF NOT EXISTS backup_leases (
    lease_key TEXT PRIMARY KEY,
    owner_id UUID NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS backup_leases_expiry_idx ON backup_leases (expires_at);

CREATE TABLE IF NOT EXISTS backup_manifests (
    backup_id TEXT PRIMARY KEY,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'uploading', 'complete', 'failed')),
    object_key TEXT NOT NULL,
    total_size_bytes BIGINT NOT NULL DEFAULT 0 CHECK (total_size_bytes >= 0),
    sha256 TEXT NOT NULL DEFAULT '',
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS backup_manifests_status_idx ON backup_manifests (status, updated_at DESC);

CREATE TABLE IF NOT EXISTS backup_manifest_parts (
    backup_id TEXT NOT NULL REFERENCES backup_manifests(backup_id) ON DELETE CASCADE,
    part_no INTEGER NOT NULL CHECK (part_no > 0),
    object_key TEXT NOT NULL,
    size_bytes BIGINT NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
    sha256 TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'uploading', 'complete', 'failed')),
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (backup_id, part_no)
);

CREATE INDEX IF NOT EXISTS backup_manifest_parts_status_idx ON backup_manifest_parts (backup_id, status, part_no);
