package httpapi

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
)

// contentSecurityPolicy is on every response (security.md, API hardening):
// the portal runs only what this origin serves, so nothing injected into a
// page runs, frames it, or sends its data elsewhere. base-uri, form-action
// and frame-ancestors don't fall back to default-src.
const contentSecurityPolicy = "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// portalPages are the portal's own URLs (portals.md, Navigation), each
// answered with its index.html. They are listed rather than falling back
// for any path, so everything else stays a 404: a path the edge must not
// route looks routed if it answers 200 (scripts/external-check.sh).
var portalPages = []string{
	"GET /{$}",
	"GET /login",
	"GET /signup",
	"GET /reset-password",
	"GET /databases",
	"GET /databases/{rest...}",
	"GET /activity",
	"GET /subscription",
	"GET /account",
	"GET /account/{rest...}",
}

// portalRoutes serves the built portal, if assets has one.
func (s *Server) portalRoutes(assets fs.FS) error {
	if assets == nil {
		return nil
	}
	index, err := fs.ReadFile(assets, "index.html")
	if errors.Is(err, fs.ErrNotExist) {
		s.log.Warn("this binary has no portal (make web): serving the API alone")
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the portal: %w", err)
	}
	page := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	}
	for _, pattern := range portalPages {
		s.mux.HandleFunc(pattern, page)
	}
	// One segment: no directory listing, and no way out of assets/.
	s.mux.HandleFunc("GET /assets/{file}", func(w http.ResponseWriter, r *http.Request) {
		name := "assets/" + r.PathValue("file")
		if !isFile(assets, name) {
			http.NotFound(w, r)
			return
		}
		// Vite puts a hash of the content in these names.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.ServeFileFS(w, r, assets, name)
	})
	if isFile(assets, "favicon.svg") {
		s.mux.HandleFunc("GET /favicon.svg", func(w http.ResponseWriter, r *http.Request) {
			http.ServeFileFS(w, r, assets, "favicon.svg")
		})
	}
	return nil
}

func isFile(assets fs.FS, name string) bool {
	info, err := fs.Stat(assets, name)
	return err == nil && info.Mode().IsRegular()
}
