package share

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/yalexaner/yacht/internal/config"
	"github.com/yalexaner/yacht/internal/db"
	"github.com/yalexaner/yacht/internal/storage/local"
)

// newTestService wires a Service onto a fresh temp-dir SQLite and a fresh
// temp-dir local storage backend. The db and storage roots live in separate
// t.TempDir() calls so a test that inspects one doesn't accidentally see
// files from the other. Only DefaultExpiry is populated on the config — the
// service doesn't read any other field at this phase, and keeping the rest
// zero makes accidental dependencies easy to spot (a nil pointer / zero value
// will fail loudly).
func newTestService(t *testing.T) (*Service, *sql.DB) {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.db")
	handle, err := db.New(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { handle.Close() })

	if _, err := db.Migrate(ctx, handle); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}

	backend, err := local.New(t.TempDir())
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}

	cfg := &config.Shared{DefaultExpiry: 24 * time.Hour}
	return New(handle, backend, cfg), handle
}

// insertTestUser inserts a row into the users table and returns the new id.
// Every CreateShare test needs a valid user_id because the shares table has
// a FOREIGN KEY on users(id). telegram_id uses the test's wall-clock nanos
// so two tests (or two users within one test) don't collide on the UNIQUE
// constraint.
func insertTestUser(t *testing.T, handle *sql.DB) int64 {
	t.Helper()

	res, err := handle.ExecContext(
		context.Background(),
		`INSERT INTO users (telegram_id, is_admin, created_at)
		 VALUES (?, 0, strftime('%s','now'))`,
		time.Now().UnixNano(),
	)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

func TestNew_Constructor(t *testing.T) {
	svc, handle := newTestService(t)
	if svc == nil {
		t.Fatal("New returned nil")
	}
	if svc.db != handle {
		t.Error("Service.db does not match the handle returned by newTestService")
	}
	if svc.storage == nil {
		t.Error("Service.storage is nil")
	}
	if svc.cfg == nil || svc.cfg.DefaultExpiry != 24*time.Hour {
		t.Errorf("Service.cfg.DefaultExpiry = %v, want 24h", svc.cfg.DefaultExpiry)
	}
}
