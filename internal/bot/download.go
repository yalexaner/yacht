package bot

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

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

// newHTTPDownloader returns a downloader backed by http.DefaultClient. Using
// the shared default client means we inherit the runtime's connection pool
// without standing up a second one for the bot binary.
func newHTTPDownloader() *httpDownloader {
	return &httpDownloader{client: http.DefaultClient}
}

// Download issues a GET against url and returns the body + ContentLength. On
// any non-2xx response the body is drained-and-closed and the HTTP status is
// folded into the returned error so handlers can log something useful without
// re-reading the response.
func (d *httpDownloader) Download(ctx context.Context, url string) (io.ReadCloser, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("httpDownloader.Download: build request: %w", err)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("httpDownloader.Download: do request: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("httpDownloader.Download: unexpected status %d (%s)", resp.StatusCode, resp.Status)
	}

	return resp.Body, resp.ContentLength, nil
}
