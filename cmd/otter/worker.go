package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ottermq/otter_cloud/internal/config"
	"github.com/ottermq/otter_cloud/internal/jaildeck"
	"github.com/ottermq/otter_cloud/internal/operations"
	"github.com/ottermq/otter_cloud/internal/platformdb"
	"github.com/ottermq/otter_cloud/internal/postgres"
)

func runWorker(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("worker", "Claim and run this node's operations", stderr)
	check := fs.Bool("check", false, "check the configuration and secrets, connect to nothing, and exit; rc.d runs it before starting")
	if code, done := parse(fs, args); done {
		return code
	}
	cfg, ok := loadConfig("worker", config.Needs{Database: true, JailDeck: true, Cluster: true, Metrics: true, Dumps: true}, stderr)
	if !ok {
		return 1
	}
	if *check {
		if _, err := postgres.LoadCredentials(cfg.ClusterCert, []byte(cfg.ClusterKey.Reveal())); err != nil {
			fmt.Fprintf(stderr, "otter worker: %v\n", err)
			return 1
		}
		if err := findPgDump(); err != nil {
			fmt.Fprintf(stderr, "otter worker: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "otter worker: configuration ok")
		return 0
	}
	log := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := work(ctx, cfg, log); err != nil {
		log.Error("otter worker stopped", "err", err)
		return 1
	}
	return 0
}

// findPgDump checks that the final dumps' pg_dump is in PATH. Without it
// every delete fails at its dump, after routing has stopped (#58).
func findPgDump() error {
	if _, err := exec.LookPath("pg_dump"); err != nil {
		return fmt.Errorf("pg_dump is not in PATH (%s): final dumps need postgresqlNN-client", os.Getenv("PATH"))
	}
	return nil
}

// work runs the node's operations until ctx is cancelled. An operation
// running then is left to its lease, and claimed again on the next start.
func work(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	creds, err := postgres.LoadCredentials(cfg.ClusterCert, []byte(cfg.ClusterKey.Reveal()))
	if err != nil {
		return err
	}
	if err := findPgDump(); err != nil {
		return err
	}
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pool, err := platformdb.NewPool(startCtx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	// LISTEN needs a connection of its own, held for the worker's life.
	listen, err := platformdb.Connect(startCtx, cfg)
	if err != nil {
		return err
	}
	defer listen.Close(context.WithoutCancel(ctx))

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	host, _ := os.Hostname()
	driver := &postgres.Driver{JailDeck: jaildeck.New(cfg.JailDeckSocket, cfg.JailDeckToken.Reveal()), Credentials: creds,
		Dumper: postgres.Dumper{CertFile: cfg.ClusterCertFile, KeyFile: cfg.ClusterKeyFile, Dir: cfg.DumpDir}}
	w := &operations.Worker{DB: pool, Name: fmt.Sprintf("%s/%d", host, os.Getpid()), Handlers: driver.Handlers(), Log: log,
		Metrics: operations.NewMetrics(reg), Reconcile: driver.Reconciler(pool)}

	if cfg.MetricsAddr != "" {
		ln, err := net.Listen("tcp", cfg.MetricsAddr)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", cfg.MetricsAddr, err)
		}
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
		srv := newHTTPServer(mux, log)
		go func() { _ = srv.Serve(ln) }()
		defer srv.Close()
		log.Info("otter worker metrics listening", "addr", ln.Addr().String())
	}
	log.Info("otter worker starting", "config", cfg)
	return w.Run(ctx, listen)
}
