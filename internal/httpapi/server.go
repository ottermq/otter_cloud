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

	"github.com/jackc/pgx/v5/pgxpool"
)

// maxBodyBytes bounds a request body. Every API request is a small JSON
// object.
const maxBodyBytes = 64 << 10

// Server is otter api's HTTP handler.
type Server struct {
	db  *pgxpool.Pool
	log *slog.Logger
	mux *http.ServeMux
}

// New returns the API server. db connects as otter_api.
func New(db *pgxpool.Pool, log *slog.Logger) *Server {
	s := &Server{db: db, log: log, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("POST /api/signup", s.handleSignUp)
	s.mux.HandleFunc("POST /api/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/logout", s.handleLogout)
	s.mux.Handle("GET /api/me", s.requireSession(http.HandlerFunc(s.handleMe)))
	// Anything else under /api/, including a known path with the wrong
	// method, gets a JSON 404 rather than the mux's plain-text one.
	s.mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint")
	})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	s.mux.ServeHTTP(w, r)
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

// clientIP is the peer's address. Behind the edge proxy this is the
// proxy's; trusting X-Forwarded-For from it comes with rate limiting (#34).
func clientIP(r *http.Request) netip.Addr {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().Unmap()
}
