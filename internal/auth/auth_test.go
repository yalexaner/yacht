package auth

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/yalexaner/yacht/internal/db"
)

// newTestDB opens a fresh on-disk SQLite (`:memory:` isn't shared across
// connections in modernc.org/sqlite, and the *sql.DB handle may dispatch
// different statements to different pooled connections), runs the embedded
// migrations, and returns the handle. The file lives under t.TempDir() so
// Go's test cleanup removes it automatically.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "auth.db")
	handle, err := db.New(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { handle.Close() })

	if _, err := db.Migrate(ctx, handle); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	return handle
}

// insertTestUser inserts a users row with the provided telegram_id and
// is_admin flag and returns the generated primary key. The unique
// telegram_id constraint means callers that need multiple users in a
// single test must pass distinct telegramIDs.
func insertTestUser(t *testing.T, handle *sql.DB, telegramID int64, isAdmin bool) int64 {
	t.Helper()

	adminFlag := 0
	if isAdmin {
		adminFlag = 1
	}
	res, err := handle.ExecContext(
		context.Background(),
		`INSERT INTO users (telegram_id, telegram_username, display_name, is_admin, created_at)
		 VALUES (?, ?, ?, ?, strftime('%s','now'))`,
		telegramID, "testuser", "Test User", adminFlag,
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

func TestLookupUserByTelegramID(t *testing.T) {
	handle := newTestDB(t)

	const (
		adminTG    int64 = 1001
		nonAdminTG int64 = 1002
		missingTG  int64 = 9999
		ruAdminTG  int64 = 1003
	)
	adminID := insertTestUser(t, handle, adminTG, true)
	insertTestUser(t, handle, nonAdminTG, false)

	// plant an admin with lang already set so we can confirm the SELECT
	// surfaces the column value through to the User struct.
	ruAdminID := insertTestUser(t, handle, ruAdminTG, true)
	if _, err := handle.ExecContext(
		context.Background(),
		`UPDATE users SET lang = ? WHERE id = ?`, "ru", ruAdminID,
	); err != nil {
		t.Fatalf("set lang on admin: %v", err)
	}

	t.Run("admin row returns user", func(t *testing.T) {
		got, err := lookupUserByTelegramID(context.Background(), handle, adminTG)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.ID != adminID {
			t.Errorf("ID = %d, want %d", got.ID, adminID)
		}
		if got.TelegramID != adminTG {
			t.Errorf("TelegramID = %d, want %d", got.TelegramID, adminTG)
		}
		if got.Username != "testuser" {
			t.Errorf("Username = %q, want %q", got.Username, "testuser")
		}
		if got.DisplayName != "Test User" {
			t.Errorf("DisplayName = %q, want %q", got.DisplayName, "Test User")
		}
		if !got.IsAdmin {
			t.Error("IsAdmin = false, want true")
		}
		if got.Lang != nil {
			t.Errorf("Lang = %q (ptr non-nil), want nil for unset column", *got.Lang)
		}
	})

	t.Run("admin row with lang set returns lang on user", func(t *testing.T) {
		got, err := lookupUserByTelegramID(context.Background(), handle, ruAdminTG)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Lang == nil {
			t.Fatal("Lang = nil, want non-nil pointing at \"ru\"")
		}
		if *got.Lang != "ru" {
			t.Errorf("Lang = %q, want %q", *got.Lang, "ru")
		}
	})

	t.Run("non-admin row returns ErrUnauthorized", func(t *testing.T) {
		got, err := lookupUserByTelegramID(context.Background(), handle, nonAdminTG)
		if got != nil {
			t.Errorf("expected nil user, got %+v", got)
		}
		if !errors.Is(err, ErrUnauthorized) {
			t.Errorf("expected ErrUnauthorized, got %v", err)
		}
	})

	t.Run("missing row returns ErrUnauthorized", func(t *testing.T) {
		got, err := lookupUserByTelegramID(context.Background(), handle, missingTG)
		if got != nil {
			t.Errorf("expected nil user, got %+v", got)
		}
		if !errors.Is(err, ErrUnauthorized) {
			t.Errorf("expected ErrUnauthorized, got %v", err)
		}
	})
}

// TestAuthProviderInterface keeps the AuthProvider contract pinned as a
// compile-only check. It uses a local stub so this test compiles even
// before the concrete providers land in later tasks. Replacing the stub
// with `*TelegramWidget` and `*BotToken` assertions in their respective
// test files is intentional — keep one here as a belt-and-braces check
// against accidental interface drift.
func TestAuthProviderInterface(t *testing.T) {
	var _ AuthProvider = (*stubProvider)(nil)
}

type stubProvider struct{}

func (stubProvider) Name() string                        { return "stub" }
func (stubProvider) Verify(*http.Request) (*User, error) { return nil, ErrUnauthorized }
