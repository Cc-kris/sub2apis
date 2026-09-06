package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// BackupRepository persists cross-instance backup coordination and manifests.
type BackupRepository struct{ db *sql.DB }

func NewBackupRepository(db *sql.DB) service.BackupRepository { return &BackupRepository{db: db} }

func (r *BackupRepository) check() error {
	if r == nil || r.db == nil {
		return fmt.Errorf("backup repository is unavailable")
	}
	return nil
}

func (r *BackupRepository) AcquireBackupLease(ctx context.Context, key, owner string, expires time.Time) (bool, error) {
	if err := r.check(); err != nil {
		return false, err
	}
	var acquiredOwner string
	err := r.db.QueryRowContext(ctx, `INSERT INTO backup_leases(lease_key,owner_id,expires_at) VALUES($1,$2::uuid,$3) ON CONFLICT(lease_key) DO UPDATE SET owner_id=EXCLUDED.owner_id,expires_at=EXCLUDED.expires_at,updated_at=NOW() WHERE backup_leases.expires_at<=NOW() OR backup_leases.owner_id=EXCLUDED.owner_id RETURNING owner_id::text`, key, owner, expires).Scan(&acquiredOwner)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return acquiredOwner == owner, nil
}

func (r *BackupRepository) RenewBackupLease(ctx context.Context, key, owner string, expires time.Time) (bool, error) {
	if err := r.check(); err != nil {
		return false, err
	}
	result, err := r.db.ExecContext(ctx, `UPDATE backup_leases SET expires_at=$3,updated_at=NOW() WHERE lease_key=$1 AND owner_id=$2::uuid`, key, owner, expires)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (r *BackupRepository) ReleaseBackupLease(ctx context.Context, key, owner string) error {
	if err := r.check(); err != nil {
		return err
	}
	_, err := r.db.ExecContext(ctx, `DELETE FROM backup_leases WHERE lease_key=$1 AND owner_id=$2::uuid`, key, owner)
	return err
}

func (r *BackupRepository) CreateBackupManifest(ctx context.Context, m service.BackupManifest) error {
	if err := r.check(); err != nil {
		return err
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO backup_manifests(backup_id,status,object_key,total_size_bytes,sha256,error_message,completed_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, m.BackupID, m.Status, m.ObjectKey, m.TotalSizeBytes, m.SHA256, m.ErrorMessage, m.CompletedAt)
	return err
}

func (r *BackupRepository) UpdateBackupManifest(ctx context.Context, id, status string, size int64, sha string, msg *string, completed *time.Time) error {
	if err := r.check(); err != nil {
		return err
	}
	_, err := r.db.ExecContext(ctx, `UPDATE backup_manifests SET status=$2,total_size_bytes=$3,sha256=$4,error_message=$5,completed_at=$6,updated_at=NOW() WHERE backup_id=$1`, id, status, size, sha, msg, completed)
	return err
}

func (r *BackupRepository) GetBackupManifest(ctx context.Context, id string) (*service.BackupManifest, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	m := &service.BackupManifest{}
	err := r.db.QueryRowContext(ctx, `SELECT backup_id,status,object_key,total_size_bytes,sha256,error_message,created_at,updated_at,completed_at FROM backup_manifests WHERE backup_id=$1`, id).Scan(&m.BackupID, &m.Status, &m.ObjectKey, &m.TotalSizeBytes, &m.SHA256, &m.ErrorMessage, &m.CreatedAt, &m.UpdatedAt, &m.CompletedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

func (r *BackupRepository) UpsertBackupManifestPart(ctx context.Context, p service.BackupManifestPart) error {
	if err := r.check(); err != nil {
		return err
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO backup_manifest_parts(backup_id,part_no,object_key,size_bytes,sha256,status,error_message) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(backup_id,part_no) DO UPDATE SET object_key=EXCLUDED.object_key,size_bytes=EXCLUDED.size_bytes,sha256=EXCLUDED.sha256,status=EXCLUDED.status,error_message=EXCLUDED.error_message,updated_at=NOW()`, p.BackupID, p.PartNo, p.ObjectKey, p.SizeBytes, p.SHA256, p.Status, p.ErrorMessage)
	return err
}

func (r *BackupRepository) ListBackupManifestParts(ctx context.Context, id string) ([]service.BackupManifestPart, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT backup_id,part_no,object_key,size_bytes,sha256,status,error_message,created_at,updated_at FROM backup_manifest_parts WHERE backup_id=$1 ORDER BY part_no`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	parts := make([]service.BackupManifestPart, 0)
	for rows.Next() {
		var p service.BackupManifestPart
		if err := rows.Scan(&p.BackupID, &p.PartNo, &p.ObjectKey, &p.SizeBytes, &p.SHA256, &p.Status, &p.ErrorMessage, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		parts = append(parts, p)
	}
	return parts, rows.Err()
}
