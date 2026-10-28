package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ottermq/otter_cloud/internal/config"
	"github.com/ottermq/otter_cloud/internal/jaildeck"
	"github.com/ottermq/otter_cloud/internal/operations"
	"github.com/ottermq/otter_cloud/internal/platformdb"
	"github.com/ottermq/otter_cloud/internal/postgres"
)

func runWorker(args []string, _, stderr io.Writer) int {
	fs := newFlagSet("worker", "Claim and run this node's operations", stderr)
	if code, done := parse(fs, args); done {
		return code
	}
	cfg, ok := loadConfig("worker", config.Needs{Database: true, JailDeck: true, Cluster: true}, stderr)
	if !ok {
		return 1
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

// work runs the node's operations until ctx is cancelled. An operation
// running then is left to its lease, and claimed again on the next start.
func work(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	creds, err := postgres.LoadCredentials(cfg.ClusterCert, []byte(cfg.ClusterKey.Reveal()))
	if err != nil {
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

	host, _ := os.Hostname()
	driver := &postgres.Driver{JailDeck: jaildeck.New(cfg.JailDeckSocket, cfg.JailDeckToken.Reveal()), Credentials: creds}
	w := &operations.Worker{DB: pool, Name: fmt.Sprintf("%s/%d", host, os.Getpid()), Handlers: driver.Handlers(), Log: log}
	log.Info("otter worker starting", "config", cfg)
	return w.Run(ctx, listen)
}
