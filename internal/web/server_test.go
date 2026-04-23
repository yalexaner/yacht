package web

import (
	"io"
	"log/slog"
	"testing"

	"github.com/yalexaner/yacht/internal/config"
)

// TestNew_ParsesTemplates is the Phase-7 Task-1 sanity check: Server.New
// should walk the embedded templates FS at construction time and produce a
// template set where every expected file is reachable via Lookup. The set
// of expected names mirrors the placeholder files we shipped in
// web/templates/; future tasks may add more, but losing one of these would
// silently break a render path that the handlers in later tasks rely on.
func TestNew_ParsesTemplates(t *testing.T) {
	cfg := &config.Web{Shared: &config.Shared{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv, err := New(cfg, nil, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, name := range []string{
		"base.html",
		"share_file.html",
		"share_text.html",
		"password.html",
		"error.html",
	} {
		if srv.templates.Lookup(name) == nil {
			t.Errorf("template %q not parsed", name)
		}
	}
}
