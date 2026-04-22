// Package factory builds the concrete storage.Storage implementation that
// matches cfg.StorageBackend. It lives in its own package (not inside
// internal/storage) because the interface package is imported by every
// backend — putting the factory alongside the interface would create a
// storage → local → storage import cycle.
//
// Call site: factory.New(ctx, cfg.Shared) from cmd/web and cmd/bot startup.
// The returned value satisfies storage.Storage; callers do not care which
// backend they got.
package factory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"

	"github.com/yalexaner/yacht/internal/config"
	"github.com/yalexaner/yacht/internal/storage"
	"github.com/yalexaner/yacht/internal/storage/local"
	"github.com/yalexaner/yacht/internal/storage/r2"
)

// New constructs the storage.Storage implementation selected by
// cfg.StorageBackend. The ctx is threaded into R2 construction because the
// aws-sdk-go-v2 credential chain honors cancellation; the local backend
// ignores it but still accepts it for signature symmetry.
//
// Errors from the concrete constructor are wrapped with a descriptive prefix
// so the startup log line ("init storage: ...") points at the backend that
// failed rather than leaking the raw SDK / filesystem error alone.
func New(ctx context.Context, cfg *config.Shared) (storage.Storage, error) {
	// defense in depth: callers in cmd/* pass a non-nil *cfg.Shared obtained
	// from config.Load{Web,Bot}, but a hand-built caller (e.g. a test or a
	// future entry point) that passes nil deserves a real error, not a nil
	// dereference panic.
	if cfg == nil {
		return nil, errors.New("init storage: nil config")
	}
	switch cfg.StorageBackend {
	case config.StorageBackendLocal:
		b, err := local.New(cfg.StorageLocalPath)
		if err != nil {
			return nil, fmt.Errorf("init local storage: %w", err)
		}
		return b, nil

	case config.StorageBackendR2:
		b, err := r2.New(ctx, cfg.R2AccountID, cfg.R2AccessKeyID, cfg.R2SecretAccessKey, cfg.R2Bucket, cfg.R2Endpoint)
		if err != nil {
			return nil, fmt.Errorf("init r2 storage: %w", err)
		}
		return b, nil

	default:
		// defense in depth: config.LoadShared already rejects unknown backend
		// values, so reaching this arm means either a hand-built *config.Shared
		// (e.g. a test) or a bug in the loader. Fail explicitly rather than
		// silently returning a nil Storage that would NPE on first use.
		return nil, fmt.Errorf("unknown storage backend %q", cfg.StorageBackend)
	}
}

// LogReady emits the one-readiness-line-per-component log that every startup
// step in cmd/web and cmd/bot uses ("<component> ready", ...). It lives next
// to New because the field set is backend-specific and already needs the same
// cfg and StorageBackend switch — having both binaries re-implement the
// switch invites drift when the field set changes.
//
// For R2, the endpoint is decomposed to the host only so credentials or path
// components (which shouldn't appear in R2_ENDPOINT but could after a future
// config change) cannot sneak into the log line.
func LogReady(logger *slog.Logger, cfg *config.Shared) {
	// symmetric with New's nil guard: a hand-built caller that passes nil
	// must not panic a startup that would otherwise succeed. LogReady returns
	// no error, so we downgrade to a warn line rather than swallowing silently.
	if cfg == nil {
		logger.Warn("storage ready skipped: nil config")
		return
	}
	switch cfg.StorageBackend {
	case config.StorageBackendLocal:
		logger.Info("storage ready",
			"backend", cfg.StorageBackend,
			"path", cfg.StorageLocalPath,
		)
	case config.StorageBackendR2:
		logger.Info("storage ready",
			"backend", cfg.StorageBackend,
			"bucket", cfg.R2Bucket,
			"endpoint_host", endpointHost(cfg.R2Endpoint),
		)
	default:
		// unreachable in production (config loader rejects unknown backends)
		// but kept so a hand-built *config.Shared in a test still emits a line
		// rather than dropping the log entirely.
		logger.Info("storage ready", "backend", cfg.StorageBackend)
	}
}

// endpointHost returns the host component of a URL-shaped endpoint, or a
// fixed "<unparseable>" marker when the input fails to parse or has no host.
// Used only for logging; we deliberately do NOT fall back to the raw string
// because a misconfigured R2_ENDPOINT could contain credentials or path
// components (e.g. "user:pass@host" missing the scheme) that would slip into
// the log line — defeating the whole point of extracting host-only. The
// factory itself is the authoritative validator of the endpoint.
func endpointHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "<unparseable>"
	}
	return u.Host
}
