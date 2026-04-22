package bot

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/yalexaner/yacht/internal/config"
	"github.com/yalexaner/yacht/internal/db"
	"github.com/yalexaner/yacht/internal/share"
	"github.com/yalexaner/yacht/internal/storage/local"
)

// fakeAPI is the test substitute for the real *tgbotapi.BotAPI. It captures
// outbound Sends so handler tests can assert on the reply payload without
// standing up the real client, and returns caller-configured values for the
// file-URL lookup used by the document/photo handlers (Task 6/7).
type fakeAPI struct {
	sent            []tgbotapi.Chattable
	sendErr         error
	fileURL         string
	fileURLErr      error
	getFileURLCalls []string
}

func (f *fakeAPI) Send(c tgbotapi.Chattable) (tgbotapi.Message, error) {
	f.sent = append(f.sent, c)
	if f.sendErr != nil {
		return tgbotapi.Message{}, f.sendErr
	}
	return tgbotapi.Message{}, nil
}

func (f *fakeAPI) GetFileDirectURL(fileID string) (string, error) {
	f.getFileURLCalls = append(f.getFileURLCalls, fileID)
	if f.fileURLErr != nil {
		return "", f.fileURLErr
	}
	return f.fileURL, nil
}

// compile-time assertion that our fake still satisfies the narrow interface —
// a regression here means the interface grew a method the fake doesn't
// implement, which would silently let tests that don't exercise the new
// method pass with a misleading stub.
var _ telegramAPI = (*fakeAPI)(nil)

// fakeDownloader serves caller-configured bytes with zero network I/O. The
// calls slice lets tests assert which URLs the handler asked to download
// (useful for document/photo tests that need to verify size-guard short-
// circuits skipped the fetch entirely).
type fakeDownloader struct {
	body  []byte
	err   error
	calls []string
}

func (f *fakeDownloader) Download(_ context.Context, url string) (io.ReadCloser, int64, error) {
	f.calls = append(f.calls, url)
	if f.err != nil {
		return nil, 0, f.err
	}
	return io.NopCloser(bytes.NewReader(f.body)), int64(len(f.body)), nil
}

var _ fileDownloader = (*fakeDownloader)(nil)

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

// testBot bundles everything a share-creating handler test needs to drive the
// bot and verify the side effects: the bot itself, the captured fakes, the
// DB handle for table inspection, and the Telegram ID registered as an admin
// (so tests can build messages that pass the admin-map lookup).
type testBot struct {
	bot      *Bot
	api      *fakeAPI
	dl       *fakeDownloader
	db       *sql.DB
	adminTG  int64
	adminRow int64
}

// newTestBot wires a Bot around a real share.Service (temp SQLite + local
// storage root under t.TempDir) plus fake Telegram I/O and an io.Discard
// logger. Parallel to share.newTestService — same real-deps-for-data-access,
// fakes-for-I/O split — so handler tests exercise the full persistence path
// without depending on a real Telegram connection.
func newTestBot(t *testing.T) *testBot {
	t.Helper()

	ctx := context.Background()
	handle := newTestDB(t)

	backend, err := local.New(t.TempDir())
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}

	cfg := &config.Bot{
		Shared: &config.Shared{
			BaseURL:        "https://yacht.example",
			DefaultExpiry:  24 * time.Hour,
			MaxUploadBytes: 10 * 1024 * 1024,
		},
	}
	svc := share.New(handle, backend, cfg.Shared)

	const adminTG = int64(77777)
	admins, err := bootstrapUsers(ctx, handle, []int64{adminTG})
	if err != nil {
		t.Fatalf("bootstrapUsers: %v", err)
	}

	api := &fakeAPI{}
	dl := &fakeDownloader{}

	b := &Bot{
		api:        api,
		downloader: dl,
		share:      svc,
		cfg:        cfg,
		admins:     admins,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return &testBot{
		bot:      b,
		api:      api,
		dl:       dl,
		db:       handle,
		adminTG:  adminTG,
		adminRow: admins[adminTG],
	}
}

// newAdminMessage builds a message authored by the test admin with the given
// chat ID and text. Handlers that go deeper (document/photo) override the
// returned message to add Document / Photo payloads.
func newAdminMessage(tb *testBot, chatID int64, text string) *tgbotapi.Message {
	return &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: chatID},
		From: &tgbotapi.User{ID: tb.adminTG},
		Text: text,
	}
}

func TestHandleText_HappyPath(t *testing.T) {
	tb := newTestBot(t)

	reply, err := tb.bot.handleText(context.Background(), newAdminMessage(tb, 42, "hello"))
	if err != nil {
		t.Fatalf("handleText: %v", err)
	}

	if reply.ChatID != 42 {
		t.Errorf("reply.ChatID = %d, want 42", reply.ChatID)
	}
	if !strings.Contains(reply.Text, "Saved as text") {
		t.Errorf("reply.Text missing text-saved marker; got %q", reply.Text)
	}
	if !strings.Contains(reply.Text, "https://yacht.example/") {
		t.Errorf("reply.Text missing URL prefix; got %q", reply.Text)
	}

	var (
		content string
		kind    string
	)
	err = tb.db.QueryRowContext(
		context.Background(),
		`SELECT kind, text_content FROM shares WHERE user_id = ?`,
		tb.adminRow,
	).Scan(&kind, &content)
	if err != nil {
		t.Fatalf("lookup persisted share: %v", err)
	}
	if kind != share.KindText {
		t.Errorf("stored kind = %q, want %q", kind, share.KindText)
	}
	if content != "hello" {
		t.Errorf("stored text_content = %q, want %q", content, "hello")
	}
}

func TestHandleText_EmptyText(t *testing.T) {
	tb := newTestBot(t)

	reply, err := tb.bot.handleText(context.Background(), newAdminMessage(tb, 42, ""))
	if err != nil {
		t.Fatalf("handleText: %v", err)
	}
	if reply.ChatID != 0 || reply.Text != "" {
		t.Errorf("want zero-value MessageConfig, got ChatID=%d text=%q", reply.ChatID, reply.Text)
	}

	var n int
	if err := tb.db.QueryRowContext(
		context.Background(),
		`SELECT COUNT(*) FROM shares`,
	).Scan(&n); err != nil {
		t.Fatalf("count shares: %v", err)
	}
	if n != 0 {
		t.Errorf("shares row count = %d, want 0 (empty text must not persist)", n)
	}
}

func TestHandleText_ShareCreationError(t *testing.T) {
	tb := newTestBot(t)

	// force CreateTextShare to fail by closing the DB handle before the
	// handler call. CreateTextShare will fail at the allocateShareID SELECT
	// or the subsequent INSERT with an "sql: database is closed" error.
	if err := tb.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	reply, err := tb.bot.handleText(context.Background(), newAdminMessage(tb, 42, "hello"))
	if err != nil {
		t.Fatalf("handleText: %v", err)
	}

	if reply.ChatID != 42 {
		t.Errorf("reply.ChatID = %d, want 42", reply.ChatID)
	}
	if !strings.Contains(strings.ToLower(reply.Text), "try again") {
		t.Errorf("reply.Text missing try-again notice; got %q", reply.Text)
	}
}

func TestHandleText_UnauthorizedSender(t *testing.T) {
	tb := newTestBot(t)

	// defense-in-depth: the dispatcher should filter unauthorized senders
	// before they reach this handler, but the handler is expected to no-op
	// if one sneaks through. Lock that in so a routing change can't leak
	// writes into the DB via this path.
	msg := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 42},
		From: &tgbotapi.User{ID: tb.adminTG + 1},
		Text: "hello",
	}

	reply, err := tb.bot.handleText(context.Background(), msg)
	if err != nil {
		t.Fatalf("handleText: %v", err)
	}
	if reply.ChatID != 0 || reply.Text != "" {
		t.Errorf("want zero-value MessageConfig, got ChatID=%d text=%q", reply.ChatID, reply.Text)
	}

	var n int
	if err := tb.db.QueryRowContext(
		context.Background(),
		`SELECT COUNT(*) FROM shares`,
	).Scan(&n); err != nil {
		t.Fatalf("count shares: %v", err)
	}
	if n != 0 {
		t.Errorf("shares row count = %d, want 0", n)
	}
}
