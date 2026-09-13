package settings

// MinIO sync settings (docs/MINIO_PLAN.md, option A). Storage keys are flat
// minio_* rows in the settings table; the secret key is stored separately and
// is write-only over the API - it mirrors the SMTP password handling (it can
// be set, never read back; the view only carries secret_set).

import (
	"context"
	"fmt"
	"strings"
)

// Storage keys (settings table).
const (
	keyMinioEnabled     = "minio_enabled"
	keyMinioEndpoint    = "minio_endpoint"
	keyMinioBucket      = "minio_bucket"
	keyMinioAccessKey   = "minio_access_key"
	keyMinioSecretKey   = "minio_secret_key" // write-only, like smtp_password
	keyMinioUseSSL      = "minio_use_ssl"
	keyMinioPrefix      = "minio_prefix"
	keyMinioConcurrency = "minio_concurrency"
)

// Defaults.
const (
	// DefaultMinioPrefix anchors object keys at {prefix}{sec_uid}/{item_id}/...
	DefaultMinioPrefix = "douyin/"
	// DefaultMinioConcurrency keeps upload bandwidth below the downloads.
	DefaultMinioConcurrency = 2
)

// MinioView is the API projection of the MinIO settings (no secret ever).
type MinioView struct {
	Enabled     bool   `json:"enabled"`
	Endpoint    string `json:"endpoint"`
	Bucket      string `json:"bucket"`
	AccessKey   string `json:"access_key"`
	SecretSet   bool   `json:"secret_set"`
	UseSSL      bool   `json:"use_ssl"`
	Prefix      string `json:"prefix"`
	Concurrency int    `json:"concurrency"`
}

// MinioPatch partially updates the MinIO settings (the "minio" object of the
// PATCH /api/settings body; nil fields are kept).
type MinioPatch struct {
	Enabled   *bool   `json:"enabled"`
	Endpoint  *string `json:"endpoint"`
	Bucket    *string `json:"bucket"`
	AccessKey *string `json:"access_key"`
	// SecretKey is write-only: setting it stores it; reading settings never
	// returns it.
	SecretKey   *string `json:"secret_key"`
	UseSSL      *bool   `json:"use_ssl"`
	Prefix      *string `json:"prefix"`
	Concurrency *int    `json:"concurrency"`
}

// MinioConfig is the full MinIO configuration for the uploader (secret
// included); it never crosses the API boundary.
type MinioConfig struct {
	Enabled     bool
	Endpoint    string
	Bucket      string
	AccessKey   string
	SecretKey   string
	UseSSL      bool
	Prefix      string
	Concurrency int
}

// Ready reports whether the configuration is complete enough to talk to a
// MinIO server (endpoint + bucket; credentials may be empty for anonymous
// servers - a wrong setup surfaces through the test button / sync status).
func (c MinioConfig) Ready() bool {
	return c.Endpoint != "" && c.Bucket != ""
}

// MinioConfig returns the full MinIO settings for the uploader, defaults
// applied (prefix "douyin/", concurrency 2). Read live on every call so a
// settings change takes effect for the next upload without a restart.
func (s *Store) MinioConfig(ctx context.Context) MinioConfig {
	values, err := s.raw(ctx)
	if err != nil {
		return MinioConfig{Prefix: DefaultMinioPrefix, Concurrency: DefaultMinioConcurrency}
	}
	return minioConfigFrom(values)
}

// MinioPrefix returns just the object key prefix (trailing slash included).
func (s *Store) MinioPrefix(ctx context.Context) string {
	return s.MinioConfig(ctx).Prefix
}

// minioConfigFrom decodes the stored rows into the full config.
func minioConfigFrom(values map[string]string) MinioConfig {
	cfg := MinioConfig{
		Enabled:     decodeBool(values[keyMinioEnabled]),
		Endpoint:    strings.TrimSpace(decodeString(values[keyMinioEndpoint])),
		Bucket:      strings.TrimSpace(decodeString(values[keyMinioBucket])),
		AccessKey:   strings.TrimSpace(decodeString(values[keyMinioAccessKey])),
		SecretKey:   decodeString(values[keyMinioSecretKey]),
		UseSSL:      decodeBool(values[keyMinioUseSSL]),
		Prefix:      DefaultMinioPrefix,
		Concurrency: DefaultMinioConcurrency,
	}
	// An explicitly stored prefix (even the empty string = bucket root) wins
	// over the default; only an absent row falls back to "douyin/".
	if raw, ok := values[keyMinioPrefix]; ok {
		cfg.Prefix = normalizeMinioPrefix(decodeString(raw))
	}
	if n := decodeInt(values[keyMinioConcurrency]); n > 0 {
		cfg.Concurrency = n
	}
	return cfg
}

// minioView builds the API projection (secret reduced to secret_set).
func minioView(values map[string]string) MinioView {
	cfg := minioConfigFrom(values)
	return MinioView{
		Enabled:     cfg.Enabled,
		Endpoint:    cfg.Endpoint,
		Bucket:      cfg.Bucket,
		AccessKey:   cfg.AccessKey,
		SecretSet:   cfg.SecretKey != "",
		UseSSL:      cfg.UseSSL,
		Prefix:      cfg.Prefix,
		Concurrency: cfg.Concurrency,
	}
}

// normalizeMinioPrefix cleans a user-supplied object prefix: trimmed, leading
// slashes stripped, exactly one trailing slash ("" = bucket root).
func normalizeMinioPrefix(raw string) string {
	p := strings.TrimLeft(strings.TrimSpace(raw), "/")
	if p == "" {
		return ""
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// applyMinio validates the minio patch and merges it into the updates map
// (called from Store.Apply before the rows are written). Enabling MinIO
// requires a non-empty endpoint - stored or patched.
func (s *Store) applyMinio(ctx context.Context, p *MinioPatch, updates map[string]string) error {
	// Merge base: the stored values (same pattern as the smtp object merge).
	current := map[string]string{}
	if values, err := s.raw(ctx); err == nil {
		current = values
	}
	enabled := decodeBool(current[keyMinioEnabled])
	endpoint := strings.TrimSpace(decodeString(current[keyMinioEndpoint]))
	if p.Enabled != nil {
		enabled = *p.Enabled
	}
	if p.Endpoint != nil {
		endpoint = strings.TrimSpace(*p.Endpoint)
	}
	if enabled && endpoint == "" {
		return fmt.Errorf("settings: minio.endpoint is required when minio is enabled")
	}

	if p.Enabled != nil {
		updates[keyMinioEnabled] = mustJSON(*p.Enabled)
	}
	if p.Endpoint != nil {
		updates[keyMinioEndpoint] = mustJSON(endpoint)
	}
	if p.Bucket != nil {
		updates[keyMinioBucket] = mustJSON(strings.TrimSpace(*p.Bucket))
	}
	if p.AccessKey != nil {
		updates[keyMinioAccessKey] = mustJSON(strings.TrimSpace(*p.AccessKey))
	}
	if p.SecretKey != nil {
		// Stored as its own key so View/MinioConfig treat it separately
		// (same convention as smtp_password).
		updates[keyMinioSecretKey] = mustJSON(*p.SecretKey)
	}
	if p.UseSSL != nil {
		updates[keyMinioUseSSL] = mustJSON(*p.UseSSL)
	}
	if p.Prefix != nil {
		updates[keyMinioPrefix] = mustJSON(normalizeMinioPrefix(*p.Prefix))
	}
	if p.Concurrency != nil {
		if *p.Concurrency < 1 || *p.Concurrency > 8 {
			return fmt.Errorf("settings: minio.concurrency must be 1-8")
		}
		updates[keyMinioConcurrency] = mustJSON(*p.Concurrency)
	}
	return nil
}
