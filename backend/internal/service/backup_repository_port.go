package service

import (
	"context"
	"time"
)

// BackupRepository is the durable coordination boundary for scheduled backups.
// Implementations must make lease acquisition a cross-instance compare-and-set.
type BackupRepository interface {
	AcquireBackupLease(ctx context.Context, leaseKey, ownerID string, expiresAt time.Time) (bool, error)
	RenewBackupLease(ctx context.Context, leaseKey, ownerID string, expiresAt time.Time) (bool, error)
	ReleaseBackupLease(ctx context.Context, leaseKey, ownerID string) error
	CreateBackupManifest(ctx context.Context, manifest BackupManifest) error
	UpdateBackupManifest(ctx context.Context, backupID, status string, totalSizeBytes int64, sha256 string, errorMessage *string, completedAt *time.Time) error
	GetBackupManifest(ctx context.Context, backupID string) (*BackupManifest, error)
	UpsertBackupManifestPart(ctx context.Context, part BackupManifestPart) error
	ListBackupManifestParts(ctx context.Context, backupID string) ([]BackupManifestPart, error)
}

type BackupManifest struct {
	BackupID       string
	Status         string
	ObjectKey      string
	TotalSizeBytes int64
	SHA256         string
	ErrorMessage   *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    *time.Time
}

type BackupManifestPart struct {
	BackupID     string
	PartNo       int
	ObjectKey    string
	SizeBytes    int64
	SHA256       string
	Status       string
	ErrorMessage *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}
