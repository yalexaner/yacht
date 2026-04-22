package r2

import (
	"context"
	"strings"
	"testing"
)

// TestNew_RejectsEmptyArgs exercises the constructor's validation path. It
// runs in the default go-test path (no //go:build integration) because it
// does not touch the network or need credentials — the check fires before
// the SDK is invoked.
func TestNew_RejectsEmptyArgs(t *testing.T) {
	const (
		accountID = "acct-123"
		accessKey = "AKIA-TEST"
		secret    = "test-secret"
		bucket    = "test-bucket"
		endpoint  = "https://acct-123.r2.cloudflarestorage.com"
	)

	tests := []struct {
		name     string
		args     []string // accessKeyID, secretAccessKey, bucket, endpoint
		wantSub  string
	}{
		{"empty access key id", []string{"", secret, bucket, endpoint}, "access key id is empty"},
		{"empty secret access key", []string{accessKey, "", bucket, endpoint}, "secret access key is empty"},
		{"empty bucket", []string{accessKey, secret, "", endpoint}, "bucket is empty"},
		{"empty endpoint", []string{accessKey, secret, bucket, ""}, "endpoint is empty"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := New(context.Background(), accountID, tc.args[0], tc.args[1], tc.args[2], tc.args[3])
			if err == nil {
				t.Fatalf("New returned nil error; want error mentioning %q", tc.wantSub)
			}
			if b != nil {
				t.Errorf("New returned non-nil backend on error path")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q; want it to contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}
