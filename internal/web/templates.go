package web

import (
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
)

// parseTemplates loads every *.html file from fsys into a single associated
// template set. The receiver template is named "" so callers must reach the
// concrete templates via Lookup(<basename>) or ExecuteTemplate(<basename>) —
// there is no implicit "default" template to fall back to, which keeps a
// missing template name from silently rendering the wrong page.
//
// Each file ends up as an associated template named after its basename
// (e.g. base.html, share_file.html). Files that contain only {{define}}
// blocks still produce a basename-named template — its body is empty, but
// Lookup returns it, so the sanity test in server_test.go can confirm the
// parser saw every file we expected.
func parseTemplates(fsys fs.FS) (*template.Template, error) {
	tmpl, err := template.New("").ParseFS(fsys, "*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return tmpl, nil
}

// render writes the standard text/html headers and executes the named
// template against data. Execution failures are logged rather than
// propagated because the response status + headers may already have been
// flushed by the time the template engine errors out — at that point the
// best we can do is leave a trail for the operator.
func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.templates.ExecuteTemplate(w, name, data); err != nil {
		s.logger.Error("render template", "name", name, "err", err)
	}
}
