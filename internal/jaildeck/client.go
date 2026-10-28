package jaildeck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

// Client talks to the JailDeck on this host over its unix socket, with a
// named token (JD-002, JD-003, JD-016). It is safe for concurrent use.
type Client struct {
	http  *http.Client
	token string
}

// New returns a client for the socket at path. The token is
// root-equivalent on this host: it is read from a 0400 file by the caller,
// never logged and never sent anywhere but the socket.
func New(socket, token string) *Client {
	var d net.Dialer
	return &Client{
		token: token,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return d.DialContext(ctx, "unix", socket)
				},
				MaxIdleConns:    2,
				IdleConnTimeout: 30 * time.Second,
			},
			// An exec can run for its whole timeout; a context bounds each call.
			Timeout: 0,
		},
	}
}

// Error is a response JailDeck refused, with its status and message.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("jaildeck: %d %s: %s", e.Status, http.StatusText(e.Status), e.Message)
}

// IsNotFound reports whether err is JailDeck's 404.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

type requestIDKey struct{}

// requestIDPattern is what JailDeck records; anything else it drops.
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// WithRequestID makes every call made with ctx send id as X-Request-Id, so
// JailDeck's audit entries carry it. The worker passes the operation id,
// which links both audit trails.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// Jail is a jail as JailDeck reports it.
type Jail struct {
	Name       string            `json:"name"`
	JID        string            `json:"jid"`
	Status     string            `json:"status"` // running or stopped
	Hostname   string            `json:"hostname"`
	IP         string            `json:"ip"`
	Path       string            `json:"path"`
	Properties map[string]string `json:"properties,omitempty"`
}

// Running reports whether the jail is running.
func (j Jail) Running() bool { return j.Status == "running" }

// Jails lists the host's jails.
func (c *Client) Jails(ctx context.Context) ([]Jail, error) {
	var js []Jail
	return js, c.do(ctx, http.MethodGet, "/api/jails", nil, &js)
}

// Jail shows one jail; IsNotFound(err) when there is none by that name.
func (c *Client) Jail(ctx context.Context, name string) (Jail, error) {
	var j Jail
	return j, c.do(ctx, http.MethodGet, "/api/jails/"+url.PathEscape(name), nil, &j)
}

// ExecRequest is a command to run as root in a jail: an argument array,
// never a shell line (JD-016).
type ExecRequest struct {
	Argv    []string          `json:"argv"`
	Env     map[string]string `json:"env,omitempty"`
	Timeout time.Duration     `json:"-"` // JailDeck's default, 60 s, when zero
}

// ExecResult is what the command did. A non-zero ExitCode is the command's
// failure, not the call's: Exec returns it without an error.
type ExecResult struct {
	ExitCode        int    `json:"exit_code"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
	TimedOut        bool   `json:"timed_out"`
	DurationMS      int64  `json:"duration_ms"`
}

// Exec runs a command in the jail.
func (c *Client) Exec(ctx context.Context, jail string, req ExecRequest) (ExecResult, error) {
	if len(req.Argv) == 0 {
		return ExecResult{}, errors.New("jaildeck: exec needs a command")
	}
	body := struct {
		ExecRequest
		Timeout string `json:"timeout,omitempty"`
	}{ExecRequest: req}
	if req.Timeout > 0 {
		body.Timeout = req.Timeout.String()
	}
	var r ExecResult
	return r, c.do(ctx, http.MethodPost, "/api/jails/"+url.PathEscape(jail)+"/exec", body, &r)
}

// AuditEntry is one command JailDeck ran.
type AuditEntry struct {
	Timestamp string `json:"timestamp"`
	Operation string `json:"operation"`
	Target    string `json:"target"`
	Command   string `json:"command"`
	Success   bool   `json:"success"`
	RequestID string `json:"request_id"`
}

// Audit lists JailDeck's audit entries for a request id, newest first.
func (c *Client) Audit(ctx context.Context, requestID string, limit int) ([]AuditEntry, error) {
	q := url.Values{"request_id": {requestID}, "limit": {strconv.Itoa(limit)}}
	var es []AuditEntry
	return es, c.do(ctx, http.MethodGet, "/api/audit?"+q.Encode(), nil, &es)
}

// maxResponse bounds what is read: an exec's output is capped by JailDeck,
// and nothing else is large.
const maxResponse = 8 << 20

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	// The host is ignored: the transport always dials the socket.
	req, err := http.NewRequestWithContext(ctx, method, "http://jaildeck"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if id, ok := ctx.Value(requestIDKey{}).(string); ok && requestIDPattern.MatchString(id) {
		req.Header.Set("X-Request-Id", id)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("jaildeck: %s %s: %w", method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("jaildeck: %s %s: %w", method, req.URL.Path, err)
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = string(bytes.TrimSpace(data[:min(len(data), 200)]))
		}
		return &Error{Status: resp.StatusCode, Message: e.Error}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("jaildeck: %s %s: decode: %w", method, req.URL.Path, err)
	}
	return nil
}
