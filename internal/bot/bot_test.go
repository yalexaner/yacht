package bot

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/yalexaner/yacht/internal/config"
	"github.com/yalexaner/yacht/internal/db"
)

// compile-time assertion that the real Telegram API satisfies our narrow
// interface — a regression here (e.g. a breaking upstream rename of Send or
// GetFileDirectURL) would silently fail in production because main wires up
// the real client without going through this interface. The var form forces
// the assertion at package-compile time, so the build fails before tests run.
var _ telegramAPI = (*tgbotapi.BotAPI)(nil)

// newTestDB opens a fresh temp-dir SQLite database with migrations applied.
// Every bot-package test that touches the users/shares tables threads its
// handle through this helper so the schema matches production exactly and
// cleanup is automatic via t.Cleanup.
func newTestDB(t *testing.T) *sql.DB {
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
	return handle
}

// readUserRow returns (rowID, isAdmin) for the given telegram_id. Returns
// -1/-1 when no row exists so tests can assert "not present" without sentinel
// plumbing.
func readUserRow(t *testing.T, handle *sql.DB, telegramID int64) (int64, int64) {
	t.Helper()

	var id, isAdmin int64
	err := handle.QueryRowContext(
		context.Background(),
		`SELECT id, is_admin FROM users WHERE telegram_id = ?`,
		telegramID,
	).Scan(&id, &isAdmin)
	if err == sql.ErrNoRows {
		return -1, -1
	}
	if err != nil {
		t.Fatalf("read user row telegram_id=%d: %v", telegramID, err)
	}
	return id, isAdmin
}

// countUserRows returns the total number of rows in the users table —
// sufficient to detect duplicate inserts across the bootstrap tests.
func countUserRows(t *testing.T, handle *sql.DB) int {
	t.Helper()

	var n int
	if err := handle.QueryRowContext(
		context.Background(),
		`SELECT COUNT(*) FROM users`,
	).Scan(&n); err != nil {
		t.Fatalf("count users: %v", err)
	}
	return n
}

func TestBootstrapUsers_FreshDB(t *testing.T) {
	handle := newTestDB(t)
	adminIDs := []int64{123456789, 987654321}

	admins, err := bootstrapUsers(context.Background(), handle, adminIDs)
	if err != nil {
		t.Fatalf("bootstrapUsers: %v", err)
	}
	if len(admins) != len(adminIDs) {
		t.Fatalf("returned map len = %d, want %d", len(admins), len(adminIDs))
	}

	for _, tgID := range adminIDs {
		rowID, isAdmin := readUserRow(t, handle, tgID)
		if rowID == -1 {
			t.Fatalf("telegram_id=%d not inserted", tgID)
		}
		if isAdmin != 1 {
			t.Errorf("telegram_id=%d is_admin = %d, want 1", tgID, isAdmin)
		}
		if admins[tgID] != rowID {
			t.Errorf("admins[%d] = %d, want %d (DB row id)", tgID, admins[tgID], rowID)
		}
	}
}

func TestBootstrapUsers_PreExistingAdmin(t *testing.T) {
	handle := newTestDB(t)
	preExisting := int64(111111111)

	res, err := handle.ExecContext(
		context.Background(),
		`INSERT INTO users (telegram_id, is_admin, created_at)
		 VALUES (?, 1, strftime('%s','now'))`,
		preExisting,
	)
	if err != nil {
		t.Fatalf("insert pre-existing admin: %v", err)
	}
	originalID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}

	admins, err := bootstrapUsers(
		context.Background(),
		handle,
		[]int64{preExisting, 222222222},
	)
	if err != nil {
		t.Fatalf("bootstrapUsers: %v", err)
	}

	if got := countUserRows(t, handle); got != 2 {
		t.Fatalf("users row count = %d, want 2 (no duplicates)", got)
	}
	if admins[preExisting] != originalID {
		t.Errorf("admins[%d] = %d, want %d (original row preserved)",
			preExisting, admins[preExisting], originalID)
	}
}

func TestBootstrapUsers_PreExistingNonAdmin(t *testing.T) {
	handle := newTestDB(t)
	preExisting := int64(333333333)

	if _, err := handle.ExecContext(
		context.Background(),
		`INSERT INTO users (telegram_id, is_admin, created_at)
		 VALUES (?, 0, strftime('%s','now'))`,
		preExisting,
	); err != nil {
		t.Fatalf("insert pre-existing non-admin: %v", err)
	}

	if _, err := bootstrapUsers(
		context.Background(),
		handle,
		[]int64{preExisting},
	); err != nil {
		t.Fatalf("bootstrapUsers: %v", err)
	}

	_, isAdmin := readUserRow(t, handle, preExisting)
	if isAdmin != 1 {
		t.Errorf("is_admin = %d, want 1 (promoted by bootstrap)", isAdmin)
	}
}

func TestBootstrapUsers_EmptyList(t *testing.T) {
	handle := newTestDB(t)

	admins, err := bootstrapUsers(context.Background(), handle, nil)
	if err == nil {
		t.Fatal("bootstrapUsers with empty list returned nil error, want error")
	}
	if admins != nil {
		t.Errorf("admins = %v, want nil on error", admins)
	}
}

// newCommandTestBot returns a Bot pared down to the fields the command
// handlers (/start, /help) read — only cfg.DefaultExpiry matters. Keeping the
// fixture minimal avoids coupling command-handler tests to the eventual share
// service + fake telegramAPI wiring that later tasks introduce.
func newCommandTestBot(t *testing.T, expiry time.Duration) *Bot {
	t.Helper()
	return &Bot{
		cfg: &config.Bot{
			Shared: &config.Shared{DefaultExpiry: expiry},
		},
	}
}

// newTestMessage builds a minimal tgbotapi.Message with a chat ID populated.
// Command handlers only read msg.Chat.ID, so leaving the other fields zeroed
// keeps the setup focused and readable.
func newTestMessage(chatID int64) *tgbotapi.Message {
	return &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: chatID}}
}

func TestHandleStart_RendersBody(t *testing.T) {
	b := newCommandTestBot(t, 24*time.Hour)

	reply, err := b.handleStart(context.Background(), newTestMessage(12345))
	if err != nil {
		t.Fatalf("handleStart: %v", err)
	}
	if reply.ChatID != 12345 {
		t.Errorf("reply.ChatID = %d, want 12345", reply.ChatID)
	}
	if !strings.Contains(reply.Text, "Send me a file or text message") {
		t.Errorf("reply.Text missing welcome prose; got %q", reply.Text)
	}
	if !strings.Contains(reply.Text, "allowlisted") {
		t.Errorf("reply.Text missing allowlist notice; got %q", reply.Text)
	}
	if !strings.Contains(reply.Text, (24 * time.Hour).String()) {
		t.Errorf("reply.Text missing DefaultExpiry %q; got %q", (24 * time.Hour).String(), reply.Text)
	}
}

func TestHandleHelp_MentionsAdminFuture(t *testing.T) {
	b := newCommandTestBot(t, 24*time.Hour)

	reply, err := b.handleHelp(context.Background(), newTestMessage(54321))
	if err != nil {
		t.Fatalf("handleHelp: %v", err)
	}
	if reply.ChatID != 54321 {
		t.Errorf("reply.ChatID = %d, want 54321", reply.ChatID)
	}
	if !strings.Contains(reply.Text, "Admin commands") {
		t.Errorf("reply.Text missing admin-future notice; got %q", reply.Text)
	}
	if !strings.Contains(reply.Text, "/allow") {
		t.Errorf("reply.Text missing /allow reference; got %q", reply.Text)
	}
}
