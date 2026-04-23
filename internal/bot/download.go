package bot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// downloadTimeout caps every Telegram file fetch. The Run loop dispatches
// updates serially, so a hung GET against a stalled Telegram edge would block
// every subsequent message until the process dies — http.Client with no
// Timeout (the default) is exactly that failure mode. Five minutes is chosen
// to be comfortably longer than a 100 MiB (default MaxUploadBytes) download
// at a slow residential uplink while still bounding the worst case.
const downloadTimeout = 5 * time.Minute

// httpDownloader is the production fileDownloader. It issues a plain GET
// against a Telegram file direct URL and streams the response body back to
// the caller untouched — no buffering, no retries.
//
// The caller owns the returned body and must Close it. ContentLength is read
// straight from the response and is the authoritative size we pass on to
// share.Service; Telegram's file URLs always populate it, so we trust it as
// the source of truth for the share row's size field.
type httpDownloader struct {
	client *http.Client
}

// compile-time assertion — keeps the production downloader in lockstep with
// the interface the rest of the bot package consumes.
var _ fileDownloader = (*httpDownloader)(nil)

// newHTTPDownloader returns a downloader backed by a dedicated http.Client
// with an explicit Timeout. We don't use http.DefaultClient because its zero
// Timeout would let a hung Telegram edge stall Run's serial dispatch loop
// indefinitely.
func newHTTPDownloader() *httpDownloader {
	return &httpDownloader{client: &http.Client{Timeout: downloadTimeout}}
}

// Download issues a GET against url and returns the body + ContentLength. On
// any non-2xx response the body is drained-and-closed and the HTTP status is
// folded into the returned error so handlers can log something useful without
// re-reading the response.
func (d *httpDownloader) Download(ctx context.Context, rawURL string) (io.ReadCloser, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("httpDownloader.Download: build request: %w", redactURL(err))
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("httpDownloader.Download: %w", redactURL(err))
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// drain before close so net/http can return the TCP connection to the
		// keep-alive pool — closing an un-drained body forces a fresh dial on
		// the next request, which adds avoidable churn if non-2xx responses
		// cluster (e.g. a token rotation round).
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil, 0, fmt.Errorf("httpDownloader.Download: unexpected status %d (%s)", resp.StatusCode, resp.Status)
	}

	return resp.Body, resp.ContentLength, nil
}

// redactURL strips the URL from a *url.Error so the bot token embedded in
// Telegram file URLs (https://api.telegram.org/file/bot<TOKEN>/...) does not
// leak into logs when a request fails. The standard library formats url.Error
// as `<Op> "<URL>": <Err>` by default, which for our call sites would include
// the token verbatim on every transient network hiccup. We preserve Op and
// the wrapped cause so errors.Is/errors.As still work as expected.
func redactURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s: %w", urlErr.Op, urlErr.Err)
	}
	return err
}
