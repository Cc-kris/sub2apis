package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// S3BackupStore implements service.BackupObjectStore using AWS S3 compatible storage
type S3BackupStore struct {
	client *s3.Client
	bucket string
}

// NewS3BackupStoreFactory returns a BackupObjectStoreFactory that creates S3-backed stores
func NewS3BackupStoreFactory() service.BackupObjectStoreFactory {
	return func(ctx context.Context, cfg *service.BackupS3Config) (service.BackupObjectStore, error) {
		region := cfg.Region
		if region == "" {
			region = "auto" // Cloudflare R2 默认 region
		}

		awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
			awsconfig.WithRegion(region),
			awsconfig.WithCredentialsProvider(
				credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
			),
		)
		if err != nil {
			return nil, fmt.Errorf("load aws config: %w", err)
		}

		client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
			if cfg.Endpoint != "" {
				o.BaseEndpoint = &cfg.Endpoint
			}
			if cfg.ForcePathStyle {
				o.UsePathStyle = true
			}
			o.APIOptions = append(o.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		})

		return &S3BackupStore{client: client, bucket: cfg.Bucket}, nil
	}
}

func (s *S3BackupStore) Upload(ctx context.Context, key string, body io.Reader, contentType string) (int64, error) {
	// 读取全部内容以获取大小（S3 PutObject 需要知道内容长度）
	// 注意：阿里云 OSS 不兼容 s3manager 分片上传的签名方式，因此使用 PutObject
	data, err := io.ReadAll(body)
	if err != nil {
		return 0, fmt.Errorf("read body: %w", err)
	}

	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      &s.bucket,
		Key:         &key,
		Body:        bytes.NewReader(data),
		ContentType: &contentType,
	})
	if err != nil {
		return 0, fmt.Errorf("S3 PutObject: %w", err)
	}
	return int64(len(data)), nil
}

// backupPartObjectKey stores parts as independent objects instead of relying
// on provider-specific multipart-upload signing. This works with AWS S3,
// Cloudflare R2 and OSS-compatible endpoints alike, while allowing a failed
// part to be retried without rewriting the completed parts.
func backupPartObjectKey(key string, partNo int) string {
	return fmt.Sprintf("%s.part-%05d", key, partNo)
}

func (s *S3BackupStore) UploadPart(ctx context.Context, key string, partNo int, body io.Reader, contentType string) (int64, string, error) {
	if partNo <= 0 {
		return 0, "", fmt.Errorf("invalid backup part number: %d", partNo)
	}
	digest := sha256.New()
	counting := &countingHashReader{reader: body, hash: digest}
	partKey := backupPartObjectKey(key, partNo)
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      &s.bucket,
		Key:         &partKey,
		Body:        counting,
		ContentType: &contentType,
	})
	if err != nil {
		return 0, "", fmt.Errorf("S3 PutObject part %d: %w", partNo, err)
	}
	return counting.n, hex.EncodeToString(digest.Sum(nil)), nil
}

func (s *S3BackupStore) DownloadPart(ctx context.Context, key string, partNo int) (io.ReadCloser, error) {
	if partNo <= 0 {
		return nil, fmt.Errorf("invalid backup part number: %d", partNo)
	}
	partKey := backupPartObjectKey(key, partNo)
	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &partKey})
	if err != nil {
		return nil, fmt.Errorf("S3 GetObject part %d: %w", partNo, err)
	}
	return result.Body, nil
}

type countingHashReader struct {
	reader io.Reader
	hash   io.Writer
	n      int64
}

func (r *countingHashReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		if _, hashErr := r.hash.Write(p[:n]); hashErr != nil {
			return n, hashErr
		}
		r.n += int64(n)
	}
	return n, err
}

func (s *S3BackupStore) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
	})
	if err != nil {
		return nil, fmt.Errorf("S3 GetObject: %w", err)
	}
	return result.Body, nil
}

func (s *S3BackupStore) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
	})
	return err
}

func (s *S3BackupStore) PresignURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	presignClient := s3.NewPresignClient(s.client)
	result, err := presignClient.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
	}, s3.WithPresignExpires(expiry))
	if err != nil {
		return "", fmt.Errorf("presign url: %w", err)
	}
	return result.URL, nil
}

func (s *S3BackupStore) HeadBucket(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: &s.bucket,
	})
	if err != nil {
		return fmt.Errorf("S3 HeadBucket failed: %w", err)
	}
	return nil
}
