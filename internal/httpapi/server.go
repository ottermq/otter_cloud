package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ottermq/otter_cloud/internal/audit"
	"github.com/ottermq/otter_cloud/internal/instances"
)

// maxBodyBytes bounds a request body. Every API request is a small JSON
// object.
const maxBodyBytes = 64 << 10

// Server is otter api's HTTP handler.
type Server struct {
	db      *pgxpool.Pool
	log     *slog.Logger
	proxies []netip.Prefix
	handler http.Handler
	mux     *http.ServeMux

	accountLimit *limiter
	ipLimit      *limiter
	metrics      *metrics
}

// Options configures a Server.
type Options struct {
	// PublicURL is the portal's origin; cross-origin checks accept it even
	// if a proxy rewrites the Host header.
	PublicURL *url.URL
	// TrustedProxies are the addresses whose X-Forwarded-For is believed.
	TrustedProxies []netip.Prefix

	now func() time.Time // for tests
}

// New returns the API server. db connects as otter_api.
func New(db *pgxpool.Pool, log *slog.Logger, opts Options) (*Server, error) {
	now := opts.now
	if now == nil {
		now = time.Now
	}
	s := &Server{
		db:           db,
		log:          slog.New(requestIDHandler{log.Handler()}),
		proxies:      opts.TrustedProxies,
		mux:          http.NewServeMux(),
		accountLimit: newLimiter(accountFailures, failureWindow, now),
		ipLimit:      newLimiter(ipFailures, failureWindow, now),
		metrics:      newMetrics(),
	}
	s.routes()

	// CSRF (security.md §6): refuse unsafe cross-origin browser requests,
	// judged by Sec-Fetch-Site or else Origin against Host. Requests with
	// neither header aren't from a browser and can't carry a victim's
	// cookie on their own. SameSite=Lax and the JSON content type are the
	// other two layers.
	cop := http.NewCrossOriginProtection()
	if opts.PublicURL != nil {
		if err := cop.AddTrustedOrigin(opts.PublicURL.Scheme + "://" + opts.PublicURL.Host); err != nil {
			return nil, fmt.Errorf("trust %s: %w", opts.PublicURL, err)
		}
	}
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.log.WarnContext(r.Context(), "cross-origin request refused",
			"method", r.Method, "path", r.URL.Path, "origin", r.Header.Get("Origin"),
			"sec_fetch_site", r.Header.Get("Sec-Fetch-Site"), "client_ip", s.clientIP(r).String())
		s.record(r, audit.Event{Actor: audit.Anonymous, Action: audit.ActionCrossOriginRefuse, Outcome: audit.Denied,
			Details: map[string]any{"method": r.Method, "path": truncate(r.URL.Path, 200),
				"origin": truncate(r.Header.Get("Origin"), 200), "sec_fetch_site": truncate(r.Header.Get("Sec-Fetch-Site"), 50)}})
		writeError(w, http.StatusForbidden, "cross-origin request refused")
	}))
	s.handler = s.observe(cop.Handler(s.mux))
	return s, nil
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("POST /api/signup", s.handleSignUp)
	s.mux.HandleFunc("POST /api/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/logout", s.handleLogout)
	s.mux.Handle("GET /api/me", s.requireSession(http.HandlerFunc(s.handleMe)))
	s.mux.Handle("GET /api/catalog", s.requireSession(http.HandlerFunc(s.handleCatalog)))
	s.mux.Handle("GET /api/operations", s.requireSession(http.HandlerFunc(s.handleOperations)))
	s.mux.Handle("GET /api/operations/{id}", s.requireSession(http.HandlerFunc(s.handleOperation)))
	s.mux.Handle("GET /api/subscription", s.requireSession(http.HandlerFunc(s.handleSubscription)))
	s.mux.Handle("GET /api/instances", s.requireSession(http.HandlerFunc(s.handleInstances)))
	s.mux.Handle("POST /api/instances", s.requireSession(http.HandlerFunc(s.handleCreateInstance)))
	s.mux.Handle("GET /api/instances/{id}", s.requireSession(http.HandlerFunc(s.handleInstance)))
	s.mux.Handle("DELETE /api/instances/{id}", s.requireSession(http.HandlerFunc(s.handleDeleteInstance)))
	s.mux.Handle("POST /api/instances/{id}/stop", s.requireSession(s.lifecycle(instances.Stop, audit.ActionInstanceStop)))
	s.mux.Handle("POST /api/instances/{id}/start", s.requireSession(s.lifecycle(instances.Start, audit.ActionInstanceStart)))
	s.mux.Handle("POST /api/instances/{id}/restart", s.requireSession(s.lifecycle(instances.Restart, audit.ActionInstanceRestart)))
	s.mux.Handle("GET /api/instances/{id}/operations", s.requireSession(http.HandlerFunc(s.handleInstanceOperations)))
	// Anything else under /api/, including a known path with the wrong
	// method, gets a JSON 404 rather than the mux's plain-text one.
	s.mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint")
	})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	s.handler.ServeHTTP(w, r)
}

// errorBody is every error response: {"error": "…"}. The message is meant
// for a person and never carries internal detail.
type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}

// internalError logs err and answers 500 without detail.
func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

var errBadRequest = errors.New("bad request")

// decode reads a JSON request body into v. It requires the JSON content
// type, which an HTML form can't send cross-origin without a preflight,
// refuses unknown fields and trailing data, and bounds the size.
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "send a JSON body with Content-Type: application/json")
		return errBadRequest
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
		return errBadRequest
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid JSON body: more than one value")
		return errBadRequest
	}
	return nil
}

// clientIP is the client's address. When the peer is a trusted proxy, it is
// the rightmost X-Forwarded-For entry that isn't itself a trusted proxy:
// entries to its left were written by the client and can say anything.
func (s *Server) clientIP(r *http.Request) netip.Addr {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	peer := ap.Addr().Unmap()
	if !s.trusted(peer) {
		return peer
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return peer // a malformed hop: stop believing the header
		}
		if a = a.Unmap(); !s.trusted(a) {
			return a
		}
	}
	return peer
}

// fromTrustedProxy reports whether the request's peer is a trusted proxy.
func (s *Server) fromTrustedProxy(r *http.Request) bool {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	return err == nil && s.trusted(ap.Addr().Unmap())
}

func (s *Server) trusted(a netip.Addr) bool {
	for _, p := range s.proxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
