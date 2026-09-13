package uploader

// objectStore abstracts the S3 surface the uploader needs. The production
// implementation wraps minio-go with a fresh client per task, so endpoint/key
// changes in the settings page apply to the very next upload. Tests
// substitute an in-memory store and drive the full queue/retry machinery.

import (
	"context"
	"mime"
	"os"
	"path/filepath"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"douyin/backend/internal/settings"
)

// Config is the connection snapshot one store instance is built from.
type Config struct {
	Endpoint  string // host:port, no scheme
	AccessKey string
	SecretKey string
	UseSSL    bool
}

// asStoreConfig projects the settings store's MinIO config onto a connection
// snapshot.
func asStoreConfig(cfg settings.MinioConfig) Config {
	return Config{
		Endpoint:  cfg.Endpoint,
		AccessKey: cfg.AccessKey,
		SecretKey: cfg.SecretKey,
		UseSSL:    cfg.UseSSL,
	}
}

type objectStore interface {
	// BucketExists reports whether the bucket is present (TestConnection).
	BucketExists(ctx context.Context, bucket string) (bool, error)
	// StatObject reports the stored object's size. A missing key is
	// found=false with a nil error (the "not found" mapping lives in the
	// implementation; the queue logic above stays error-shape agnostic).
	StatObject(ctx context.Context, bucket, key string) (size int64, found bool, err error)
	// PutObject uploads the local file verbatim.
	PutObject(ctx context.Context, bucket, key, localPath string) error
}

// newMinioStore builds the production store: a fresh minio-go client.
func newMinioStore(cfg Config) (objectStore, error) {
	cli, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, err
	}
	return &minioStore{cli: cli}, nil
}

type minioStore struct {
	cli *minio.Client
}

func (m *minioStore) BucketExists(ctx context.Context, bucket string) (bool, error) {
	return m.cli.BucketExists(ctx, bucket)
}

func (m *minioStore) StatObject(ctx context.Context, bucket, key string) (int64, bool, error) {
	info, err := m.cli.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
	if err != nil {
		resp := minio.ToErrorResponse(err)
		// NoSuchKey (S3) / NotFound (some S3-compatible servers) mean the
		// key is absent - that is the normal first-upload case, not a
		// failure. Missing buckets (NoSuchBucket) stay real errors so they
		// surface in the sync status.
		if resp.Code == "NoSuchKey" || resp.Code == "NotFound" {
			return 0, false, nil
		}
		return 0, false, err
	}
	return info.Size, true, nil
}

func (m *minioStore) PutObject(ctx context.Context, bucket, key, localPath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	contentType := mime.TypeByExtension(filepath.Ext(localPath))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err = m.cli.PutObject(ctx, bucket, key, f, fi.Size(), minio.PutObjectOptions{
		ContentType: contentType,
	})
	return err
}
