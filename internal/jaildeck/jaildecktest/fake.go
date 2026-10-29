// Package jaildecktest is a fake JailDeck for unit tests: the endpoints the
// jaildeck client uses, with JailDeck's wire shapes and refusals, over a
// real unix socket. What it can't stand in for, the contract tests check
// against the real thing (internal/jaildeck/contract_test.go).
package jaildecktest

import (
	"encoding/json"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ottermq/otter_cloud/internal/jaildeck"
)

// Token is the only token the fake accepts.
const Token = "jdt_test_token"

// Exec answers an exec in a jail. The default, Emulate, knows a few
// commands.
type Exec func(jail string, argv []string, env map[string]string, timeout time.Duration) jaildeck.ExecResult

// Server is a fake JailDeck.
type Server struct {
	Socket string

	mu    sync.Mutex
	jails map[string]jaildeck.Jail
	exec  Exec
	audit []jaildeck.AuditEntry
}

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// New starts a fake JailDeck with the given jails, closed when the test
// ends.
func New(t *testing.T, jails ...jaildeck.Jail) *Server {
	t.Helper()
	// A short directory: a socket path is limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "jd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	s := &Server{Socket: filepath.Join(dir, "jd.sock"), jails: map[string]jaildeck.Jail{}}
	for _, j := range jails {
		s.jails[j.Name] = j
	}
	ln, err := net.Listen("unix", s.Socket)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/jails", s.list)
	mux.HandleFunc("GET /api/jails/{name}", s.get)
	mux.HandleFunc("POST /api/jails/{name}/exec", s.execute)
	mux.HandleFunc("GET /api/audit", s.auditList)
	srv := &http.Server{Handler: s.auth(mux), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return s
}

// Client is a jaildeck client for this server.
func (s *Server) Client() *jaildeck.Client { return jaildeck.New(s.Socket, Token) }

// OnExec sets how execs are answered.
func (s *Server) OnExec(f Exec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exec = f
}

// SetJail adds or replaces a jail.
func (s *Server) SetJail(j jaildeck.Jail) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jails[j.Name] = j
}

// AuditLog is every entry recorded so far, oldest first.
func (s *Server) AuditLog() []jaildeck.AuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]jaildeck.AuditEntry(nil), s.audit...)
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+Token {
			fail(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) list(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []jaildeck.Jail{}
	for _, j := range s.jails {
		out = append(out, j)
	}
	reply(w, out)
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jails[r.PathValue("name")]
	if !ok {
		fail(w, http.StatusNotFound, "jail not found")
		return
	}
	reply(w, j)
}

func (s *Server) execute(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Argv    []string          `json:"argv"`
		Env     map[string]string `json:"env"`
		Timeout string            `json:"timeout"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Argv) == 0 {
		fail(w, http.StatusBadRequest, "argv is required")
		return
	}
	if req.Timeout != "" {
		if _, err := time.ParseDuration(req.Timeout); err != nil {
			fail(w, http.StatusBadRequest, "timeout is not a duration")
			return
		}
	}
	name := r.PathValue("name")
	s.mu.Lock()
	j, ok := s.jails[name]
	exec := s.exec
	s.mu.Unlock()
	switch {
	case !ok:
		fail(w, http.StatusNotFound, "jail not found")
		return
	case !j.Running():
		fail(w, http.StatusConflict, "jail is not running")
		return
	}
	timeout := 60 * time.Second
	if req.Timeout != "" {
		timeout, _ = time.ParseDuration(req.Timeout)
	}
	if exec == nil {
		exec = Emulate
	}
	start := time.Now()
	res := exec(name, req.Argv, req.Env, timeout)
	if res.DurationMS == 0 {
		res.DurationMS = time.Since(start).Milliseconds()
	}
	// JailDeck audits the exec as done, whatever the command's exit code or
	// a timeout, and records what the command did (v0.18.1, jaildeck#141).
	s.record(r, jaildeck.AuditEntry{Operation: "exec", Target: name, Command: jexecLine(name, req.Env, req.Argv), Success: true,
		ExitCode: res.ExitCode, TimedOut: res.TimedOut, DurationMS: res.DurationMS})
	reply(w, res)
}

func (s *Server) auditList(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("request_id")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []jaildeck.AuditEntry{}
	for i := len(s.audit) - 1; i >= 0; i-- {
		if id == "" || s.audit[i].RequestID == id {
			out = append(out, s.audit[i])
		}
		if limit > 0 && len(out) == limit {
			break
		}
	}
	reply(w, out)
}

// record writes an audit entry, with the request id when it is one JailDeck
// keeps: anything else is dropped, not refused.
func (s *Server) record(r *http.Request, e jaildeck.AuditEntry) {
	e.RequestID = r.Header.Get("X-Request-Id")
	if !requestIDPattern.MatchString(e.RequestID) {
		e.RequestID = ""
	}
	e.Timestamp = time.Now().UTC().Format(time.RFC3339)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = append(s.audit, e)
}

// Emulate runs a few commands the way a jail would: echo, printenv,
// `sh -c "exit N"`, sleep (against the timeout) and true. Anything else
// isn't found.
func Emulate(_ string, argv []string, env map[string]string, timeout time.Duration) jaildeck.ExecResult {
	args := argv[1:]
	switch argv[0] {
	case "true":
		return jaildeck.ExecResult{}
	case "echo":
		return jaildeck.ExecResult{Stdout: strings.Join(args, " ") + "\n"}
	case "printenv":
		if v, ok := env[args[0]]; ok && len(args) == 1 {
			return jaildeck.ExecResult{Stdout: v + "\n"}
		}
		return jaildeck.ExecResult{ExitCode: 1}
	case "sh":
		if len(args) == 2 && args[0] == "-c" && strings.HasPrefix(args[1], "exit ") {
			n, _ := strconv.Atoi(strings.TrimPrefix(args[1], "exit "))
			return jaildeck.ExecResult{ExitCode: n}
		}
	case "sleep":
		if d, err := time.ParseDuration(args[0] + "s"); err == nil && d > timeout {
			// Killed at the timeout, which is how long it ran.
			return jaildeck.ExecResult{TimedOut: true, ExitCode: -1, DurationMS: timeout.Milliseconds()}
		}
		return jaildeck.ExecResult{}
	}
	return jaildeck.ExecResult{ExitCode: 127, Stderr: argv[0] + ": not found\n"}
}

// jexecLine is the command as JailDeck's audit entry records it: the jexec
// line, with the environment through env(1), words joined by spaces
// (measured against v0.18.1 by the contract test's run).
func jexecLine(jail string, env map[string]string, argv []string) string {
	words := []string{"jexec", "-l", jail}
	if len(env) > 0 {
		words = append(words, "/usr/bin/env")
		for _, k := range slices.Sorted(maps.Keys(env)) {
			words = append(words, k+"="+env[k])
		}
	}
	return strings.Join(append(words, argv...), " ")
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
