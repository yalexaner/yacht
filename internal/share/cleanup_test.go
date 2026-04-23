package share

import (
	"context"
	"testing"
)

// TestCleanup_EmptyTables locks in the baseline contract: running a GC pass
// against a freshly migrated database with no rows returns a zero-value
// CleanupStats and no error. Each subsequent task in Phase 8 layers a new
// deletion path on top; this test ensures none of them spuriously bump a
// counter or surface a false-positive error when there's nothing to do.
func TestCleanup_EmptyTables(t *testing.T) {
	svc, _ := newTestService(t)

	stats, err := svc.Cleanup(context.Background())
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if stats.SharesDeleted != 0 {
		t.Errorf("SharesDeleted = %d, want 0", stats.SharesDeleted)
	}
	if stats.SessionsDeleted != 0 {
		t.Errorf("SessionsDeleted = %d, want 0", stats.SessionsDeleted)
	}
	if stats.LoginTokensDeleted != 0 {
		t.Errorf("LoginTokensDeleted = %d, want 0", stats.LoginTokensDeleted)
	}
	if stats.Errors != 0 {
		t.Errorf("Errors = %d, want 0", stats.Errors)
	}
}

// TestCleanupStats_String locks the compact one-liner format so future edits
// don't silently change what ad-hoc debugging output or log messages look
// like. The cmd/web ticker uses structured slog attrs rather than this
// helper, but the helper is part of the package surface.
func TestCleanupStats_String(t *testing.T) {
	got := CleanupStats{
		SharesDeleted:      2,
		SessionsDeleted:    3,
		LoginTokensDeleted: 4,
		Errors:             1,
	}.String()
	want := "shares=2 sessions=3 login_tokens=4 errors=1"
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
