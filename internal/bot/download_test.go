package bot

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHTTPDownloader_HappyPath(t *testing.T) {
	payload := []byte("hello, yacht!")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	d := newHTTPDownloader()
	body, size, err := d.Download(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	t.Cleanup(func() { body.Close() })

	if size != int64(len(payload)) {
		t.Errorf("size = %d, want %d", size, len(payload))
	}

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("body = %q, want %q", got, payload)
	}
}

func TestHTTPDownloader_Non2XX(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	t.Cleanup(srv.Close)

	d := newHTTPDownloader()
	body, size, err := d.Download(context.Background(), srv.URL)
	if err == nil {
		// ensure no leak if this ever regresses
		if body != nil {
			body.Close()
		}
		t.Fatal("Download: nil error on 404 response, want error")
	}
	if body != nil {
		t.Errorf("body = %v, want nil on error", body)
	}
	if size != 0 {
		t.Errorf("size = %d, want 0 on error", size)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %q, want it to mention status 404", err)
	}
}

func TestHTTPDownloader_RedactsURLInErrors(t *testing.T) {
	// url.Error.Error() formats as `<Op> "<URL>": <Err>` by default. Telegram
	// file URLs embed the bot token (https://api.telegram.org/file/bot<TOKEN>/...),
	// so any transient network error would leak the token into logs via the
	// default wrap. Download must strip the URL before wrapping.
	const secretURL = "https://api.telegram.org/file/bot12345:SUPERSECRET/file.txt"

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := newHTTPDownloader().Download(ctx, secretURL)
	if err == nil {
		t.Fatal("Download returned nil error with pre-cancelled ctx, want error")
	}
	if strings.Contains(err.Error(), "SUPERSECRET") {
		t.Errorf("error leaks bot token: %v", err)
	}
	if strings.Contains(err.Error(), "api.telegram.org") {
		t.Errorf("error leaks URL host: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error no longer wraps context.Canceled after sanitization: %v", err)
	}
}

func TestHTTPDownloader_ContextCancel(t *testing.T) {
	// server that blocks until the request context is cancelled — simulates a
	// slow Telegram file backend. Using request.Context lets the handler
	// unblock as soon as the client cancels rather than hanging past the test
	// deadline.
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, _, err := newHTTPDownloader().Download(ctx, srv.URL)
		errCh <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("server never received request")
	}
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Download returned nil error after ctx cancel, want context.Canceled")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Download error = %v, want wrapping context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Download did not return after ctx cancel")
	}
}

