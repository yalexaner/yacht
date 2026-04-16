package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"
)

// StorageBackend values recognised by LoadShared. The app supports exactly
// these two backends; other values are rejected during validation.
const (
	StorageBackendR2    = "r2"
	StorageBackendLocal = "local"
)

// Default values applied by LoadShared when the corresponding env var is
// unset or empty. Kept as package constants so tests can reference them
// without duplicating literals.
const (
	defaultLang          = "en"
	defaultExpiryHours   = 24
	defaultMaxUploadSize = 104857600 // 100 MiB
	defaultStorage       = StorageBackendR2
)

// Shared holds every configuration field that both the web and bot binaries
// need. It is populated by LoadShared from the process environment.
//
// Secrets (R2AccessKeyID, R2SecretAccessKey) are stored verbatim so downstream
// code that needs them can use them; LogSafe is responsible for masking them
// when emitting structured logs.
type Shared struct {
	BaseURL  string
	BrandURL string

	DBPath string

	DefaultLang    string
	DefaultExpiry  time.Duration // parsed from DEFAULT_EXPIRY_HOURS
	MaxUploadBytes int64

	StorageBackend   string // StorageBackendR2 or StorageBackendLocal
	StorageLocalPath string // populated only when StorageBackend == local

	R2AccountID       string
	R2AccessKeyID     string
	R2SecretAccessKey string
	R2Bucket          string
	R2Endpoint        string
}

// LoadShared reads the Shared configuration from environment variables.
// Every parsing or validation failure is collected and returned as a single
// joined error (via errors.Join) so operators see every problem on the first
// run instead of fixing one variable at a time.
func LoadShared() (*Shared, error) {
	var (
		cfg  Shared
		errs []error
	)

	// required string vars — collect *MissingVarError into the aggregate.
	if v, err := envStringRequired("BASE_URL"); err != nil {
		errs = append(errs, err)
	} else {
		cfg.BaseURL = v
	}
	if v, err := envStringRequired("BRAND_URL"); err != nil {
		errs = append(errs, err)
	} else {
		cfg.BrandURL = v
	}
	if v, err := envStringRequired("DB_PATH"); err != nil {
		errs = append(errs, err)
	} else {
		cfg.DBPath = v
	}

	// URL validation for the two URL fields — only run when a value was set,
	// so we do not emit a redundant "invalid URL" error on an empty string.
	if cfg.BaseURL != "" {
		if err := validateURL("BASE_URL", cfg.BaseURL); err != nil {
			errs = append(errs, err)
		}
	}
	if cfg.BrandURL != "" {
		if err := validateURL("BRAND_URL", cfg.BrandURL); err != nil {
			errs = append(errs, err)
		}
	}

	// defaulted fields.
	cfg.DefaultLang = envString("DEFAULT_LANG", defaultLang)

	if d, err := envDurationHours("DEFAULT_EXPIRY_HOURS", time.Duration(defaultExpiryHours)*time.Hour); err != nil {
		errs = append(errs, err)
	} else if d <= 0 {
		// zero or negative expiry would create already-expired shares on
		// every upload; surface this as a config error instead of shipping
		// silently-broken behavior.
		errs = append(errs, fmt.Errorf("env var %q: must be positive, got %d hours", "DEFAULT_EXPIRY_HOURS", int64(d/time.Hour)))
	} else {
		cfg.DefaultExpiry = d
	}

	if n, err := envInt64("MAX_UPLOAD_BYTES", defaultMaxUploadSize); err != nil {
		errs = append(errs, err)
	} else if n <= 0 {
		// zero or negative cap effectively disables uploads — reject so the
		// operator notices at startup rather than at first 413.
		errs = append(errs, fmt.Errorf("env var %q: must be positive, got %d", "MAX_UPLOAD_BYTES", n))
	} else {
		cfg.MaxUploadBytes = n
	}

	cfg.StorageBackend = envString("STORAGE_BACKEND", defaultStorage)

	// conditional validation based on the storage backend. An unknown backend
	// is surfaced as its own error and short-circuits the backend-specific
	// requirement checks — those checks would be meaningless without a known
	// backend and would only add noise to the aggregate error.
	switch cfg.StorageBackend {
	case StorageBackendLocal:
		if v, err := envStringRequired("STORAGE_LOCAL_PATH"); err != nil {
			errs = append(errs, err)
		} else {
			cfg.StorageLocalPath = v
		}
	case StorageBackendR2:
		for _, spec := range []struct {
			name string
			dst  *string
		}{
			{"R2_ACCOUNT_ID", &cfg.R2AccountID},
			{"R2_ACCESS_KEY_ID", &cfg.R2AccessKeyID},
			{"R2_SECRET_ACCESS_KEY", &cfg.R2SecretAccessKey},
			{"R2_BUCKET", &cfg.R2Bucket},
			{"R2_ENDPOINT", &cfg.R2Endpoint},
		} {
			if v, err := envStringRequired(spec.name); err != nil {
				errs = append(errs, err)
			} else {
				*spec.dst = v
			}
		}
	default:
		errs = append(errs, fmt.Errorf(
			"env var %q: invalid storage backend %q (want %q or %q)",
			"STORAGE_BACKEND", cfg.StorageBackend,
			StorageBackendR2, StorageBackendLocal,
		))
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return &cfg, nil
}

// validateURL returns an error when s is not parseable as a URL, or when the
// parsed URL is missing a scheme or host. The var name is included in the
// error for easier operator diagnosis in aggregated output.
func validateURL(name, s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("env var %q: invalid URL %q: %w", name, s, err)
	}
	if u.Scheme == "" {
		return fmt.Errorf("env var %q: URL %q is missing a scheme", name, s)
	}
	if u.Host == "" {
		return fmt.Errorf("env var %q: URL %q is missing a host", name, s)
	}
	return nil
}

// LogSafe emits one INFO slog record named "config.shared" with every field
// of the Shared config as an attribute. Secrets (R2 access key id + secret)
// are passed through maskSecret so they never land in logs verbatim.
func (c *Shared) LogSafe(logger *slog.Logger) {
	logger.Info("config.shared",
		"base_url", c.BaseURL,
		"brand_url", c.BrandURL,
		"db_path", c.DBPath,
		"default_lang", c.DefaultLang,
		"default_expiry", c.DefaultExpiry.String(),
		"max_upload_bytes", c.MaxUploadBytes,
		"storage_backend", c.StorageBackend,
		"storage_local_path", c.StorageLocalPath,
		"r2_account_id", c.R2AccountID,
		"r2_access_key_id", maskSecret(c.R2AccessKeyID),
		"r2_secret_access_key", maskSecret(c.R2SecretAccessKey),
		"r2_bucket", c.R2Bucket,
		"r2_endpoint", c.R2Endpoint,
	)
}
