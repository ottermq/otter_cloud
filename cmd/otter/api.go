package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/ottermq/otter_cloud/internal/config"
	"github.com/ottermq/otter_cloud/internal/httpapi"
	"github.com/ottermq/otter_cloud/internal/platformdb"
)

// shutdownTimeout is how long in-flight requests get to finish on SIGTERM.
const shutdownTimeout = 15 * time.Second

func runAPI(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("api", "Serve the control plane's HTTP API and portal", stderr)
	check := fs.Bool("check", false, "check the configuration and secrets, connect to nothing, and exit; rc.d runs it before starting")
	if code, done := parse(fs, args); done {
		return code
	}
	cfg, ok := loadConfig("api", config.Needs{Database: true, HTTP: true, PublicURL: true}, stderr)
	if !ok {
		return 1
	}
	if *check {
		fmt.Fprintln(stdout, "otter api: configuration ok")
		return 0
	}
	log := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := serveAPI(ctx, cfg, log); err != nil {
		log.Error("otter api stopped", "err", err)
		return 1
	}
	return 0
}

// serveAPI serves until ctx is cancelled, then drains in-flight requests.
func serveAPI(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	pool, err := platformdb.NewPool(startCtx, cfg)
	cancel()
	if err != nil {
		return err
	}
	defer pool.Close()

	handler, err := httpapi.New(pool, log, httpapi.Options{
		PublicURL:      cfg.PublicURL,
		TrustedProxies: cfg.TrustedProxies,
	})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
	}
	servers := []*http.Server{newHTTPServer(handler, log)}
	listeners := []net.Listener{ln}
	log.Info("otter api listening", "addr", ln.Addr().String(), "config", cfg)

	if cfg.MetricsAddr != "" {
		mln, err := net.Listen("tcp", cfg.MetricsAddr)
		if err != nil {
			ln.Close()
			return fmt.Errorf("listen on %s: %w", cfg.MetricsAddr, err)
		}
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", handler.MetricsHandler())
		servers = append(servers, newHTTPServer(mux, log))
		listeners = append(listeners, mln)
		log.Info("otter api metrics listening", "addr", mln.Addr().String())
	}

	errc := make(chan error, len(servers))
	for i, srv := range servers {
		go func() { errc <- srv.Serve(listeners[i]) }()
	}
	var serveErr error
	select {
	case serveErr = <-errc:
	case <-ctx.Done():
	}
	log.Info("otter api shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil && serveErr == nil {
			serveErr = fmt.Errorf("shut down: %w", err)
		}
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}

func newHTTPServer(h http.Handler, log *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    32 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}
