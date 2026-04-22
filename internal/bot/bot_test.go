package bot

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
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
//
// updates is the channel Run reads from; tests pre-load it (and usually
// close it after seeding) so Run drains the seeded updates and then exits
// via ctx cancellation. stopCalled records whether Run invoked
// StopReceivingUpdates on shutdown — the production lib relies on it to
// stop the upstream poll goroutine, so a regression here would silently
// leak a goroutine in production.
type fakeAPI struct {
	sent            []tgbotapi.Chattable
	sendErr         error
	fileURL         string
	fileURLErr      error
	getFileURLCalls []string
	updates         chan tgbotapi.Update
	stopCalled      bool
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

func (f *fakeAPI) GetUpdatesChan(_ tgbotapi.UpdateConfig) tgbotapi.UpdatesChannel {
	if f.updates == nil {
		f.updates = make(chan tgbotapi.Update)
	}
	return f.updates
}

func (f *fakeAPI) StopReceivingUpdates() {
	f.stopCalled = true
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

// newDocumentMessage builds an admin-authored message carrying a Document
// payload. Callers set doc fields (FileID, FileName, MimeType, FileSize) on
// the returned Document to shape the specific test scenario.
func newDocumentMessage(tb *testBot, chatID int64, doc *tgbotapi.Document) *tgbotapi.Message {
	msg := newAdminMessage(tb, chatID, "")
	msg.Document = doc
	return msg
}

func TestHandleDocument_HappyPath(t *testing.T) {
	tb := newTestBot(t)
	tb.api.fileURL = "https://api.telegram.org/file/bot_token/documents/file_123"
	tb.dl.body = []byte("hello world payload")

	doc := &tgbotapi.Document{
		FileID:   "doc_abc",
		FileName: "report.txt",
		MimeType: "text/plain",
		FileSize: len(tb.dl.body),
	}

	reply, err := tb.bot.handleDocument(context.Background(), newDocumentMessage(tb, 42, doc))
	if err != nil {
		t.Fatalf("handleDocument: %v", err)
	}

	if reply.ChatID != 42 {
		t.Errorf("reply.ChatID = %d, want 42", reply.ChatID)
	}
	if !strings.Contains(reply.Text, "report.txt") {
		t.Errorf("reply.Text missing filename; got %q", reply.Text)
	}
	if !strings.Contains(reply.Text, "https://yacht.example/") {
		t.Errorf("reply.Text missing URL prefix; got %q", reply.Text)
	}

	if len(tb.api.getFileURLCalls) != 1 || tb.api.getFileURLCalls[0] != "doc_abc" {
		t.Errorf("GetFileDirectURL calls = %v, want one call for %q",
			tb.api.getFileURLCalls, "doc_abc")
	}
	if len(tb.dl.calls) != 1 || tb.dl.calls[0] != tb.api.fileURL {
		t.Errorf("downloader calls = %v, want one call for %q", tb.dl.calls, tb.api.fileURL)
	}

	var (
		shareID, kind, filename, mime string
		size                          int64
	)
	err = tb.db.QueryRowContext(
		context.Background(),
		`SELECT id, kind, original_filename, mime_type, size_bytes FROM shares WHERE user_id = ?`,
		tb.adminRow,
	).Scan(&shareID, &kind, &filename, &mime, &size)
	if err != nil {
		t.Fatalf("lookup persisted share: %v", err)
	}
	if kind != share.KindFile {
		t.Errorf("stored kind = %q, want %q", kind, share.KindFile)
	}
	if filename != "report.txt" {
		t.Errorf("stored filename = %q, want %q", filename, "report.txt")
	}
	if mime != "text/plain" {
		t.Errorf("stored mime = %q, want %q", mime, "text/plain")
	}
	if size != int64(len(tb.dl.body)) {
		t.Errorf("stored size = %d, want %d", size, len(tb.dl.body))
	}

	sh, err := tb.bot.share.Get(context.Background(), shareID)
	if err != nil {
		t.Fatalf("share.Get: %v", err)
	}
	rc, err := tb.bot.share.OpenContent(context.Background(), sh)
	if err != nil {
		t.Fatalf("OpenContent: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read storage object: %v", err)
	}
	if !bytes.Equal(got, tb.dl.body) {
		t.Errorf("storage bytes = %q, want %q", got, tb.dl.body)
	}
}

func TestHandleDocument_TooLarge(t *testing.T) {
	tb := newTestBot(t)
	tb.bot.cfg.MaxUploadBytes = 100

	doc := &tgbotapi.Document{
		FileID:   "big_file",
		FileName: "big.bin",
		FileSize: 1000,
	}

	reply, err := tb.bot.handleDocument(context.Background(), newDocumentMessage(tb, 42, doc))
	if err != nil {
		t.Fatalf("handleDocument: %v", err)
	}

	if reply.ChatID != 42 {
		t.Errorf("reply.ChatID = %d, want 42", reply.ChatID)
	}
	if !strings.Contains(strings.ToLower(reply.Text), "too large") {
		t.Errorf("reply.Text missing too-large notice; got %q", reply.Text)
	}

	if len(tb.api.getFileURLCalls) != 0 {
		t.Errorf("GetFileDirectURL called despite size guard: %v", tb.api.getFileURLCalls)
	}
	if len(tb.dl.calls) != 0 {
		t.Errorf("downloader called despite size guard: %v", tb.dl.calls)
	}

	var n int
	if err := tb.db.QueryRowContext(
		context.Background(),
		`SELECT COUNT(*) FROM shares`,
	).Scan(&n); err != nil {
		t.Fatalf("count shares: %v", err)
	}
	if n != 0 {
		t.Errorf("shares row count = %d, want 0 (oversized must not persist)", n)
	}
}

func TestHandleDocument_DownloadError(t *testing.T) {
	tb := newTestBot(t)
	tb.api.fileURL = "https://api.telegram.org/file/bot_token/documents/file_err"
	tb.dl.err = errors.New("simulated download failure")

	doc := &tgbotapi.Document{
		FileID:   "doc_err",
		FileName: "report.txt",
		FileSize: 1024,
	}

	reply, err := tb.bot.handleDocument(context.Background(), newDocumentMessage(tb, 42, doc))
	if err != nil {
		t.Fatalf("handleDocument: %v", err)
	}

	if reply.ChatID != 42 {
		t.Errorf("reply.ChatID = %d, want 42", reply.ChatID)
	}
	if !strings.Contains(strings.ToLower(reply.Text), "try again") {
		t.Errorf("reply.Text missing try-again notice; got %q", reply.Text)
	}

	var n int
	if err := tb.db.QueryRowContext(
		context.Background(),
		`SELECT COUNT(*) FROM shares`,
	).Scan(&n); err != nil {
		t.Fatalf("count shares: %v", err)
	}
	if n != 0 {
		t.Errorf("shares row count = %d, want 0 (failed download must not persist)", n)
	}
}

func TestHandleDocument_ShareCreationError(t *testing.T) {
	tb := newTestBot(t)
	tb.api.fileURL = "https://api.telegram.org/file/bot_token/documents/file_ok"
	tb.dl.body = []byte("test payload")

	doc := &tgbotapi.Document{
		FileID:   "doc_share_err",
		FileName: "report.txt",
		MimeType: "text/plain",
		FileSize: len(tb.dl.body),
	}

	// force CreateFileShare to fail at allocateShareID by closing the DB
	// handle before the call — mirrors TestHandleText_ShareCreationError.
	if err := tb.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	reply, err := tb.bot.handleDocument(context.Background(), newDocumentMessage(tb, 42, doc))
	if err != nil {
		t.Fatalf("handleDocument: %v", err)
	}

	if reply.ChatID != 42 {
		t.Errorf("reply.ChatID = %d, want 42", reply.ChatID)
	}
	if !strings.Contains(strings.ToLower(reply.Text), "try again") {
		t.Errorf("reply.Text missing try-again notice; got %q", reply.Text)
	}
}

// newPhotoMessage builds an admin-authored message carrying a Photo payload.
// Callers pass the PhotoSize slice to shape the specific test scenario (single
// size for happy path, multiple sizes for the largest-picker test).
func newPhotoMessage(tb *testBot, chatID int64, photos []tgbotapi.PhotoSize) *tgbotapi.Message {
	msg := newAdminMessage(tb, chatID, "")
	msg.Photo = photos
	return msg
}

func TestHandlePhoto_HappyPath(t *testing.T) {
	tb := newTestBot(t)
	tb.api.fileURL = "https://api.telegram.org/file/bot_token/photos/file_321"
	tb.dl.body = []byte("pretend-jpeg-bytes")

	photos := []tgbotapi.PhotoSize{
		{
			FileID:       "photo_abc",
			FileUniqueID: "unique_xyz",
			Width:        1024,
			Height:       768,
			FileSize:     len(tb.dl.body),
		},
	}

	reply, err := tb.bot.handlePhoto(context.Background(), newPhotoMessage(tb, 42, photos))
	if err != nil {
		t.Fatalf("handlePhoto: %v", err)
	}

	if reply.ChatID != 42 {
		t.Errorf("reply.ChatID = %d, want 42", reply.ChatID)
	}
	if !strings.Contains(reply.Text, "unique_xyz.jpg") {
		t.Errorf("reply.Text missing synthesised filename; got %q", reply.Text)
	}
	if !strings.Contains(reply.Text, "https://yacht.example/") {
		t.Errorf("reply.Text missing URL prefix; got %q", reply.Text)
	}

	var (
		shareID, kind, filename, mime string
		size                          int64
	)
	err = tb.db.QueryRowContext(
		context.Background(),
		`SELECT id, kind, original_filename, mime_type, size_bytes FROM shares WHERE user_id = ?`,
		tb.adminRow,
	).Scan(&shareID, &kind, &filename, &mime, &size)
	if err != nil {
		t.Fatalf("lookup persisted share: %v", err)
	}
	if kind != share.KindFile {
		t.Errorf("stored kind = %q, want %q", kind, share.KindFile)
	}
	if filename != "unique_xyz.jpg" {
		t.Errorf("stored filename = %q, want %q", filename, "unique_xyz.jpg")
	}
	if mime != "image/jpeg" {
		t.Errorf("stored mime = %q, want %q", mime, "image/jpeg")
	}
	if size != int64(len(tb.dl.body)) {
		t.Errorf("stored size = %d, want %d", size, len(tb.dl.body))
	}

	sh, err := tb.bot.share.Get(context.Background(), shareID)
	if err != nil {
		t.Fatalf("share.Get: %v", err)
	}
	rc, err := tb.bot.share.OpenContent(context.Background(), sh)
	if err != nil {
		t.Fatalf("OpenContent: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read storage object: %v", err)
	}
	if !bytes.Equal(got, tb.dl.body) {
		t.Errorf("storage bytes = %q, want %q", got, tb.dl.body)
	}
}

func TestHandlePhoto_PicksLargest(t *testing.T) {
	tb := newTestBot(t)
	tb.api.fileURL = "https://api.telegram.org/file/bot_token/photos/file_largest"
	tb.dl.body = []byte("largest-bytes")

	// Telegram orders PhotoSize entries smallest-first. The handler must pick
	// the last one — any other choice silently downgrades the sender's upload.
	photos := []tgbotapi.PhotoSize{
		{FileID: "photo_small", FileUniqueID: "u_small", Width: 90, Height: 60, FileSize: 500},
		{FileID: "photo_medium", FileUniqueID: "u_medium", Width: 320, Height: 240, FileSize: 5000},
		{FileID: "photo_large", FileUniqueID: "u_large", Width: 1280, Height: 960, FileSize: len(tb.dl.body)},
	}

	if _, err := tb.bot.handlePhoto(context.Background(), newPhotoMessage(tb, 42, photos)); err != nil {
		t.Fatalf("handlePhoto: %v", err)
	}

	if len(tb.api.getFileURLCalls) != 1 || tb.api.getFileURLCalls[0] != "photo_large" {
		t.Errorf("GetFileDirectURL calls = %v, want one call for %q",
			tb.api.getFileURLCalls, "photo_large")
	}

	var filename string
	err := tb.db.QueryRowContext(
		context.Background(),
		`SELECT original_filename FROM shares WHERE user_id = ?`,
		tb.adminRow,
	).Scan(&filename)
	if err != nil {
		t.Fatalf("lookup persisted share: %v", err)
	}
	if filename != "u_large.jpg" {
		t.Errorf("stored filename = %q, want %q (largest PhotoSize's FileUniqueID)",
			filename, "u_large.jpg")
	}
}

func TestHandlePhoto_TooLarge(t *testing.T) {
	tb := newTestBot(t)
	tb.bot.cfg.MaxUploadBytes = 100

	photos := []tgbotapi.PhotoSize{
		{FileID: "photo_small", FileUniqueID: "u_small", FileSize: 50},
		{FileID: "photo_large", FileUniqueID: "u_large", FileSize: 1000},
	}

	reply, err := tb.bot.handlePhoto(context.Background(), newPhotoMessage(tb, 42, photos))
	if err != nil {
		t.Fatalf("handlePhoto: %v", err)
	}

	if reply.ChatID != 42 {
		t.Errorf("reply.ChatID = %d, want 42", reply.ChatID)
	}
	if !strings.Contains(strings.ToLower(reply.Text), "too large") {
		t.Errorf("reply.Text missing too-large notice; got %q", reply.Text)
	}

	if len(tb.api.getFileURLCalls) != 0 {
		t.Errorf("GetFileDirectURL called despite size guard: %v", tb.api.getFileURLCalls)
	}
	if len(tb.dl.calls) != 0 {
		t.Errorf("downloader called despite size guard: %v", tb.dl.calls)
	}

	var n int
	if err := tb.db.QueryRowContext(
		context.Background(),
		`SELECT COUNT(*) FROM shares`,
	).Scan(&n); err != nil {
		t.Fatalf("count shares: %v", err)
	}
	if n != 0 {
		t.Errorf("shares row count = %d, want 0 (oversized must not persist)", n)
	}
}

// newCommandUpdate builds a /<cmd> update as Telegram would deliver it: the
// message text is the full slash-prefixed command and a bot_command entity at
// offset 0 spans the command token. Without the entity Message.IsCommand()
// returns false and the dispatcher routes to the text handler instead, which
// defeats the purpose of the command-dispatch tests.
func newCommandUpdate(tb *testBot, chatID int64, cmd string) tgbotapi.Update {
	text := "/" + cmd
	return tgbotapi.Update{
		Message: &tgbotapi.Message{
			Chat: &tgbotapi.Chat{ID: chatID},
			From: &tgbotapi.User{ID: tb.adminTG},
			Text: text,
			Entities: []tgbotapi.MessageEntity{
				{Type: "bot_command", Offset: 0, Length: len(text)},
			},
		},
	}
}

func TestHandleUpdate_NilMessage(t *testing.T) {
	tb := newTestBot(t)

	tb.bot.handleUpdate(context.Background(), tgbotapi.Update{})
	tb.bot.handleUpdate(context.Background(), tgbotapi.Update{
		Message: &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 1}, Text: "hello"},
	})

	if len(tb.api.sent) != 0 {
		t.Errorf("api.Send called %d times, want 0 (nil-guard skipped)", len(tb.api.sent))
	}
}

func TestHandleUpdate_Unauthorized(t *testing.T) {
	tb := newTestBot(t)

	var logBuf bytes.Buffer
	tb.bot.logger = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	update := tgbotapi.Update{
		Message: &tgbotapi.Message{
			Chat:     &tgbotapi.Chat{ID: 42},
			From:     &tgbotapi.User{ID: tb.adminTG + 1, UserName: "intruder"},
			Text:     "hello",
		},
	}

	tb.bot.handleUpdate(context.Background(), update)

	if len(tb.api.sent) != 0 {
		t.Errorf("api.Send called for unauthorized user: %d sends", len(tb.api.sent))
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "unauthorized") {
		t.Errorf("log missing unauthorized marker; got %q", logged)
	}
	if !strings.Contains(logged, "intruder") {
		t.Errorf("log missing username; got %q", logged)
	}
}

func TestHandleUpdate_DispatchesCommand(t *testing.T) {
	tb := newTestBot(t)

	tb.bot.handleUpdate(context.Background(), newCommandUpdate(tb, 42, "start"))

	if len(tb.api.sent) != 1 {
		t.Fatalf("api.Send called %d times, want 1", len(tb.api.sent))
	}
	sent, ok := tb.api.sent[0].(tgbotapi.MessageConfig)
	if !ok {
		t.Fatalf("sent payload = %T, want tgbotapi.MessageConfig", tb.api.sent[0])
	}
	if sent.ChatID != 42 {
		t.Errorf("sent.ChatID = %d, want 42", sent.ChatID)
	}
	if !strings.Contains(sent.Text, "Send me a file or text message") {
		t.Errorf("sent.Text missing /start welcome body; got %q", sent.Text)
	}
}

func TestHandleUpdate_UnknownCommandIgnored(t *testing.T) {
	tb := newTestBot(t)

	// /status is not one of the two MVP commands; dispatcher should drop it
	// without replying. Lock this in so accidentally wiring an unknown-command
	// fallback later doesn't silently leak replies to non-existent handlers.
	tb.bot.handleUpdate(context.Background(), newCommandUpdate(tb, 42, "status"))

	if len(tb.api.sent) != 0 {
		t.Errorf("api.Send called for unknown command: %d sends", len(tb.api.sent))
	}
}

func TestHandleUpdate_DispatchesDocument(t *testing.T) {
	tb := newTestBot(t)
	tb.api.fileURL = "https://api.telegram.org/file/bot_token/documents/file_disp"
	tb.dl.body = []byte("dispatched-doc")

	update := tgbotapi.Update{
		Message: &tgbotapi.Message{
			Chat: &tgbotapi.Chat{ID: 42},
			From: &tgbotapi.User{ID: tb.adminTG},
			Document: &tgbotapi.Document{
				FileID:   "doc_disp",
				FileName: "dispatched.txt",
				MimeType: "text/plain",
				FileSize: len(tb.dl.body),
			},
		},
	}

	tb.bot.handleUpdate(context.Background(), update)

	if len(tb.api.sent) != 1 {
		t.Fatalf("api.Send called %d times, want 1", len(tb.api.sent))
	}
	sent := tb.api.sent[0].(tgbotapi.MessageConfig)
	if !strings.Contains(sent.Text, "dispatched.txt") {
		t.Errorf("reply missing filename; got %q", sent.Text)
	}

	var kind string
	if err := tb.db.QueryRowContext(
		context.Background(),
		`SELECT kind FROM shares WHERE user_id = ?`,
		tb.adminRow,
	).Scan(&kind); err != nil {
		t.Fatalf("lookup persisted share: %v", err)
	}
	if kind != share.KindFile {
		t.Errorf("stored kind = %q, want %q (document handler should have run)", kind, share.KindFile)
	}
}

func TestHandleUpdate_DispatchesPhoto(t *testing.T) {
	tb := newTestBot(t)
	tb.api.fileURL = "https://api.telegram.org/file/bot_token/photos/file_disp"
	tb.dl.body = []byte("dispatched-photo")

	update := tgbotapi.Update{
		Message: &tgbotapi.Message{
			Chat: &tgbotapi.Chat{ID: 42},
			From: &tgbotapi.User{ID: tb.adminTG},
			Photo: []tgbotapi.PhotoSize{
				{FileID: "photo_disp", FileUniqueID: "u_disp", FileSize: len(tb.dl.body)},
			},
		},
	}

	tb.bot.handleUpdate(context.Background(), update)

	if len(tb.api.sent) != 1 {
		t.Fatalf("api.Send called %d times, want 1", len(tb.api.sent))
	}
	sent := tb.api.sent[0].(tgbotapi.MessageConfig)
	if !strings.Contains(sent.Text, "u_disp.jpg") {
		t.Errorf("reply missing synthesised filename; got %q", sent.Text)
	}

	var filename string
	if err := tb.db.QueryRowContext(
		context.Background(),
		`SELECT original_filename FROM shares WHERE user_id = ?`,
		tb.adminRow,
	).Scan(&filename); err != nil {
		t.Fatalf("lookup persisted share: %v", err)
	}
	if filename != "u_disp.jpg" {
		t.Errorf("stored filename = %q, want %q (photo handler should have run)", filename, "u_disp.jpg")
	}
}

func TestHandleUpdate_DispatchesText(t *testing.T) {
	tb := newTestBot(t)

	update := tgbotapi.Update{
		Message: &tgbotapi.Message{
			Chat: &tgbotapi.Chat{ID: 42},
			From: &tgbotapi.User{ID: tb.adminTG},
			Text: "routed-text",
		},
	}

	tb.bot.handleUpdate(context.Background(), update)

	if len(tb.api.sent) != 1 {
		t.Fatalf("api.Send called %d times, want 1", len(tb.api.sent))
	}
	sent := tb.api.sent[0].(tgbotapi.MessageConfig)
	if !strings.Contains(sent.Text, "Saved as text") {
		t.Errorf("reply missing text-saved marker; got %q", sent.Text)
	}

	var (
		kind    string
		content string
	)
	if err := tb.db.QueryRowContext(
		context.Background(),
		`SELECT kind, text_content FROM shares WHERE user_id = ?`,
		tb.adminRow,
	).Scan(&kind, &content); err != nil {
		t.Fatalf("lookup persisted share: %v", err)
	}
	if kind != share.KindText {
		t.Errorf("stored kind = %q, want %q", kind, share.KindText)
	}
	if content != "routed-text" {
		t.Errorf("stored text_content = %q, want %q", content, "routed-text")
	}
}

func TestHandleUpdate_PriorityOrder(t *testing.T) {
	tb := newTestBot(t)
	tb.api.fileURL = "https://api.telegram.org/file/bot_token/documents/file_priority"
	tb.dl.body = []byte("file-wins")

	// Document + caption in the same message: file wins, caption is ignored.
	// Caption-as-password is explicitly deferred per SPEC § Open Questions,
	// so the text must not leak into a separate text share.
	update := tgbotapi.Update{
		Message: &tgbotapi.Message{
			Chat: &tgbotapi.Chat{ID: 42},
			From: &tgbotapi.User{ID: tb.adminTG},
			Text: "ignored caption",
			Document: &tgbotapi.Document{
				FileID:   "doc_priority",
				FileName: "priority.bin",
				MimeType: "application/octet-stream",
				FileSize: len(tb.dl.body),
			},
		},
	}

	tb.bot.handleUpdate(context.Background(), update)

	var (
		n    int
		kind string
	)
	if err := tb.db.QueryRowContext(
		context.Background(),
		`SELECT COUNT(*) FROM shares`,
	).Scan(&n); err != nil {
		t.Fatalf("count shares: %v", err)
	}
	if n != 1 {
		t.Errorf("shares row count = %d, want 1 (only document share should persist)", n)
	}
	if err := tb.db.QueryRowContext(
		context.Background(),
		`SELECT kind FROM shares WHERE user_id = ?`,
		tb.adminRow,
	).Scan(&kind); err != nil {
		t.Fatalf("lookup persisted share: %v", err)
	}
	if kind != share.KindFile {
		t.Errorf("stored kind = %q, want %q (document should win over caption)", kind, share.KindFile)
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

func TestRun_HandlesUpdate(t *testing.T) {
	tb := newTestBot(t)

	// pre-load one update into a buffered channel and close it; Run will
	// drain the seeded update, observe the closed channel, and return.
	// Using close-then-read instead of timing a sleep keeps the test
	// deterministic — no goroutine race between the send and the
	// ctx-cancel path.
	ch := make(chan tgbotapi.Update, 1)
	ch <- tgbotapi.Update{
		Message: &tgbotapi.Message{
			Chat: &tgbotapi.Chat{ID: 42},
			From: &tgbotapi.User{ID: tb.adminTG},
			Text: "loop-routed",
		},
	}
	close(ch)
	tb.api.updates = ch

	if err := tb.bot.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(tb.api.sent) != 1 {
		t.Fatalf("api.Send called %d times, want 1 (update was not dispatched)", len(tb.api.sent))
	}
	sent, ok := tb.api.sent[0].(tgbotapi.MessageConfig)
	if !ok {
		t.Fatalf("sent payload = %T, want tgbotapi.MessageConfig", tb.api.sent[0])
	}
	if !strings.Contains(sent.Text, "Saved as text") {
		t.Errorf("reply missing text-saved marker; got %q", sent.Text)
	}
	if !tb.api.stopCalled {
		t.Error("Run returned without invoking StopReceivingUpdates (would leak the upstream poll goroutine)")
	}
}

func TestRun_ContextCancel(t *testing.T) {
	tb := newTestBot(t)

	// unbuffered, never-closed channel: Run blocks on either side of the
	// select, so the only way it can exit is the ctx.Done() branch. With
	// the ctx already cancelled before Run starts, that branch wins
	// immediately and Run returns context.Canceled.
	tb.api.updates = make(chan tgbotapi.Update)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := tb.bot.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
	if !tb.api.stopCalled {
		t.Error("Run returned without invoking StopReceivingUpdates")
	}
	if len(tb.api.sent) != 0 {
		t.Errorf("api.Send called %d times despite no updates, want 0", len(tb.api.sent))
	}
}
