package repository

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func newBackupRepoMock(t *testing.T) (*BackupRepository, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	return &BackupRepository{db: db}, mock, func() { _ = db.Close() }
}

func TestBackupRepositoryAcquireLeaseCompetitionAndTakeover(t *testing.T) {
	r, mock, done := newBackupRepoMock(t)
	defer done()
	expires := time.Now().Add(90 * time.Second)
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO backup_leases")).WithArgs("scheduled-backup", "11111111-1111-1111-1111-111111111111", expires).WillReturnRows(sqlmock.NewRows([]string{"owner_id"}).AddRow("11111111-1111-1111-1111-111111111111"))
	ok, err := r.AcquireBackupLease(context.Background(), "scheduled-backup", "11111111-1111-1111-1111-111111111111", expires)
	require.NoError(t, err)
	require.True(t, ok)
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO backup_leases")).WithArgs("scheduled-backup", "22222222-2222-2222-2222-222222222222", expires).WillReturnError(sql.ErrNoRows)
	ok, err = r.AcquireBackupLease(context.Background(), "scheduled-backup", "22222222-2222-2222-2222-222222222222", expires)
	require.NoError(t, err)
	require.False(t, ok)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBackupRepositoryManifestAndParts(t *testing.T) {
	r, mock, done := newBackupRepoMock(t)
	defer done()
	m := service.BackupManifest{BackupID: "b-1", Status: "pending", ObjectKey: "backups/b-1", SHA256: "abc"}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO backup_manifests")).WithArgs(m.BackupID, m.Status, m.ObjectKey, m.TotalSizeBytes, m.SHA256, m.ErrorMessage, m.CompletedAt).WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, r.CreateBackupManifest(context.Background(), m))
	p := service.BackupManifestPart{BackupID: "b-1", PartNo: 1, ObjectKey: "backups/b-1/1", SHA256: "def", Status: "complete"}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO backup_manifest_parts")).WithArgs(p.BackupID, p.PartNo, p.ObjectKey, p.SizeBytes, p.SHA256, p.Status, p.ErrorMessage).WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, r.UpsertBackupManifestPart(context.Background(), p))
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT backup_id,part_no,object_key,size_bytes,sha256,status,error_message,created_at,updated_at FROM backup_manifest_parts")).WithArgs("b-1").WillReturnRows(sqlmock.NewRows([]string{"backup_id", "part_no", "object_key", "size_bytes", "sha256", "status", "error_message", "created_at", "updated_at"}).AddRow("b-1", 1, p.ObjectKey, int64(3), p.SHA256, p.Status, nil, now, now))
	parts, err := r.ListBackupManifestParts(context.Background(), "b-1")
	require.NoError(t, err)
	require.Len(t, parts, 1)
	require.Equal(t, 1, parts[0].PartNo)
	require.NoError(t, mock.ExpectationsWereMet())
}
