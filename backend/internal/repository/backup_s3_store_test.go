package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/require"
)

func newTestS3BackupStore(t *testing.T, handler http.HandlerFunc) (*S3BackupStore, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	client := s3.NewFromConfig(aws.Config{
		Region:      "test-region",
		Credentials: credentials.NewStaticCredentialsProvider("test-access", "test-secret", ""),
		HTTPClient:  server.Client(),
	}, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(server.URL)
		options.UsePathStyle = true
		options.APIOptions = append(options.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
		options.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})
	return &S3BackupStore{client: client, bucket: "backup-bucket"}, server
}

func TestS3BackupStoreUploadPartStreamsAndHashes(t *testing.T) {
	payload := bytes.Repeat([]byte("backup-stream-"), 4096)
	wantDigest := sha256.Sum256(payload)
	var uploaded []byte
	store, server := newTestS3BackupStore(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPut, r.Method)
		require.Equal(t, "/backup-bucket/daily/database.sql.gz.part-00003", r.URL.Path)
		var err error
		uploaded, err = io.ReadAll(r.Body)
		require.NoError(t, err)
		w.WriteHeader(http.StatusOK)
	})
	defer server.Close()

	size, digest, err := store.UploadPart(context.Background(), "daily/database.sql.gz", 3, bytes.NewReader(payload), "application/gzip")
	require.NoError(t, err)
	require.Equal(t, int64(len(payload)), size)
	require.Equal(t, hex.EncodeToString(wantDigest[:]), digest)
	require.Equal(t, payload, uploaded)
}

func TestS3BackupStoreDownloadPartUsesDeterministicPartKey(t *testing.T) {
	store, server := newTestS3BackupStore(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/backup-bucket/daily/database.sql.gz.part-00012", r.URL.Path)
		_, _ = io.WriteString(w, "part-data")
	})
	defer server.Close()

	body, err := store.DownloadPart(context.Background(), "daily/database.sql.gz", 12)
	require.NoError(t, err)
	defer body.Close()
	data, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, "part-data", string(data))
}

func TestS3BackupStorePartRejectsInvalidNumberBeforeRequest(t *testing.T) {
	store, server := newTestS3BackupStore(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid part number must not reach object storage")
	})
	defer server.Close()

	_, _, err := store.UploadPart(context.Background(), "backup", 0, strings.NewReader("data"), "application/octet-stream")
	require.ErrorContains(t, err, "invalid backup part number")
	_, err = store.DownloadPart(context.Background(), "backup", -1)
	require.ErrorContains(t, err, "invalid backup part number")
}
