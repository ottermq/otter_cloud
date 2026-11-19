package jaildeck_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/ottermq/otter_cloud/internal/jaildeck"
)

// serveSocket answers JailDeck's API on a unix socket with h.
func serveSocket(t *testing.T, h http.HandlerFunc) *jaildeck.Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "jd.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return jaildeck.New(sock, "jdt_test")
}

func TestReclone(t *testing.T) {
	var got struct {
		path, confirm, template string
	}
	status := http.StatusOK
	var body any = map[string]any{"from": "tank/t/pg18@base", "to": "tank/t/pg18-new@base",
		"old_root": "tank/retained/otc-x", "children": []string{"tank/j/otc-x/data", "tank/j/otc-x/etc"}, "running": true}
	c := serveSocket(t, func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Template string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		got.path, got.confirm, got.template = r.URL.Path, r.URL.Query().Get("confirm"), in.Template
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})
	ctx := context.Background()

	r, err := c.Reclone(ctx, "otc-x", "tank/t/pg18-new@base")
	if err != nil || r.OldRoot != "tank/retained/otc-x" || len(r.Children) != 2 || !r.Running {
		t.Fatalf("Reclone = %+v, %v", r, err)
	}
	if got.path != "/api/jails/otc-x/reclone" || got.confirm != "otc-x" || got.template != "tank/t/pg18-new@base" {
		t.Errorf("asked %+v", got)
	}

	// Failed after the datasets moved: where they are comes back with the
	// error, which is what the operator needs.
	status = http.StatusInternalServerError
	body = map[string]any{"failed_at": "clone", "parked": []map[string]string{{"was": "tank/j/otc-x/data", "now": "tank/retained/data"}}}
	r, err = c.Reclone(ctx, "otc-x", "tank/t/pg18-new@base")
	var e *jaildeck.Error
	if !errors.As(err, &e) || e.Status != 500 || r.FailedAt != "clone" || len(r.Parked) != 1 || r.Parked[0].Now != "tank/retained/data" {
		t.Errorf("partial = %+v, %v", r, err)
	}

	// Refused before anything moved: JailDeck's message.
	status = http.StatusConflict
	body = map[string]string{"error": "template is superseded by pg18-newer"}
	if _, err = c.Reclone(ctx, "otc-x", "tank/t/pg18@base"); !errors.As(err, &e) || e.Status != 409 || e.Message != "template is superseded by pg18-newer" {
		t.Errorf("refused = %v", err)
	}
}

func TestTemplate(t *testing.T) {
	c := serveSocket(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/templates/pg18-new" {
			http.Error(w, `{"error":"no such template"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"name":"pg18-new","snapshot":"tank/t/pg18-new@base","state":"ready"}`))
	})
	tm, err := c.Template(context.Background(), "pg18-new")
	if err != nil || tm.Snapshot != "tank/t/pg18-new@base" || tm.State != "ready" {
		t.Errorf("Template = %+v, %v", tm, err)
	}
	if _, err := c.Template(context.Background(), "nope"); !jaildeck.IsNotFound(err) {
		t.Errorf("missing = %v", err)
	}
}
