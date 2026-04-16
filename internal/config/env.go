// Package config loads and validates yacht's runtime configuration from env
// vars. It exposes typed structs for the Shared / Web / Bot scopes plus small
// stdlib-only parsing helpers used by the loaders in this package.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// MissingVarError indicates that a required environment variable was either
// unset or set to an empty string. It is returned by envStringRequired and
// used by the per-scope loaders so aggregation layers can distinguish a
// missing-var error from a parse error if ever needed.
type MissingVarError struct {
	Name string
}

func (e *MissingVarError) Error() string {
	return fmt.Sprintf("required env var %q is not set", e.Name)
}

// envString returns the value of the named env var, or def if the var is
// unset or set to an empty string.
func envString(name, def string) string {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return def
	}
	return v
}

// envStringRequired returns the value of the named env var. When the var is
// unset or empty, it returns a non-nil *MissingVarError; otherwise the error
// pointer is nil.
func envStringRequired(name string) (string, *MissingVarError) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return "", &MissingVarError{Name: name}
	}
	return v, nil
}

// envInt parses the named env var as a base-10 int. Returns def if the var
// is unset or empty. Returns a descriptive error when the value fails to
// parse.
func envInt(name string, def int) (int, error) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("env var %q: invalid int %q: %w", name, v, err)
	}
	return n, nil
}

// envInt64 parses the named env var as a base-10 int64. Returns def if the
// var is unset or empty.
func envInt64(name string, def int64) (int64, error) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("env var %q: invalid int64 %q: %w", name, v, err)
	}
	return n, nil
}

// envDurationHours parses the named env var as an integer number of hours
// and returns the equivalent time.Duration. Returns def if unset or empty.
func envDurationHours(name string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("env var %q: invalid hours %q: %w", name, v, err)
	}
	return time.Duration(n) * time.Hour, nil
}

// envDurationDays parses the named env var as an integer number of days and
// returns the equivalent time.Duration (days * 24h). Returns def if unset or
// empty.
func envDurationDays(name string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("env var %q: invalid days %q: %w", name, v, err)
	}
	return time.Duration(n) * 24 * time.Hour, nil
}

// envBool parses the named env var as a boolean. Accepts the literal values
// "1", "0", "true", "false" (case-insensitive). Returns def if unset or
// empty. Any other value produces an error.
func envBool(name string, def bool) (bool, error) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return def, nil
	}
	switch strings.ToLower(v) {
	case "1", "true":
		return true, nil
	case "0", "false":
		return false, nil
	default:
		return false, fmt.Errorf("env var %q: invalid bool %q (want 1/0/true/false)", name, v)
	}
}

// envInt64List parses the named env var as a separator-delimited list of
// int64 values. Each element is whitespace-trimmed before parsing so values
// like "1, 2, 3" are accepted. An unset, empty, or all-whitespace variable
// produces an error: callers that want the list to be optional should gate
// the call themselves.
func envInt64List(name, sep string) ([]int64, error) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return nil, fmt.Errorf("env var %q: required list is empty", name)
	}
	parts := strings.Split(v, sep)
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("env var %q: invalid int64 %q: %w", name, p, err)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("env var %q: required list is empty", name)
	}
	return out, nil
}

// maskSecret returns a redacted rendering of s safe for logs. Strings of
// length <= 4 runes become "****"; longer strings reveal only the last 4
// runes prefixed by "****" so operators can cross-check with a known secret
// without leaking it in full. Rune-based (not byte-based) so a non-ASCII
// suffix is never sliced mid-codepoint.
func maskSecret(s string) string {
	r := []rune(s)
	if len(r) <= 4 {
		return "****"
	}
	return "****" + string(r[len(r)-4:])
}
