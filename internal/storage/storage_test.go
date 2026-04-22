package storage

import (
	"errors"
	"fmt"
	"testing"
)

// TestErrNotFound_IdentityMatch locks in the trivial base case: the sentinel
// matches itself via errors.Is. Guards against someone accidentally
// reassigning the sentinel or redefining it as a type with a broken Is().
func TestErrNotFound_IdentityMatch(t *testing.T) {
	if !errors.Is(ErrNotFound, ErrNotFound) {
		t.Fatalf("errors.Is(ErrNotFound, ErrNotFound) = false, want true")
	}
}

// TestErrNotFound_WrappedMatch is the real load-bearing assertion: backends
// are expected to wrap ErrNotFound with context (key name, backend name,
// etc.) using fmt.Errorf with %w. This test locks in that such wrapping
// remains errors.Is-compatible with the sentinel. If a future refactor
// replaces %w with %v or reassigns ErrNotFound to a non-wrappable type,
// this test fails and forces an intentional decision.
func TestErrNotFound_WrappedMatch(t *testing.T) {
	wrapped := fmt.Errorf("get %q: %w", "abc123", ErrNotFound)

	if !errors.Is(wrapped, ErrNotFound) {
		t.Fatalf("errors.Is(wrapped, ErrNotFound) = false, want true (wrapped=%v)", wrapped)
	}
}

// TestErrNotFound_DistinctFromOther makes sure the sentinel is not a
// universal match — a random other error must not accidentally satisfy
// errors.Is(err, ErrNotFound). This catches the degenerate case where
// ErrNotFound gets redefined as a type whose Is() returns true for
// everything.
func TestErrNotFound_DistinctFromOther(t *testing.T) {
	other := errors.New("some unrelated failure")

	if errors.Is(other, ErrNotFound) {
		t.Fatalf("errors.Is(other, ErrNotFound) = true, want false (other=%v)", other)
	}
}
