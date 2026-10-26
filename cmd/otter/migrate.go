package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/ottermq/otter_cloud/internal/config"
	"github.com/ottermq/otter_cloud/internal/migrate"
	"github.com/ottermq/otter_cloud/internal/platformdb"
)

// migrateTimeout bounds a whole run. Migrations are small; one that takes
// longer than this is waiting on a lock and should be looked at.
const migrateTimeout = 5 * time.Minute

func runMigrate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("migrate", "Apply the platform database's migrations, as otter_migrator", stderr)
	status := fs.Bool("status", false, "list migrations and whether each is applied; change nothing")
	if code, done := parse(fs, args); done {
		return code
	}
	cfg, ok := loadConfig("migrate", config.Needs{Database: true}, stderr)
	if !ok {
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), migrateTimeout)
	defer cancel()

	ms, err := migrate.Migrations()
	if err != nil {
		fmt.Fprintf(stderr, "otter migrate: %v\n", err)
		return 1
	}
	conn, err := platformdb.Connect(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "otter migrate: %v\n", err)
		return 1
	}
	defer conn.Close(context.Background())

	if *status {
		list, err := migrate.List(ctx, conn, ms)
		for _, s := range list {
			state := "pending"
			if s.AppliedAt != nil {
				state = "applied " + s.AppliedAt.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(stdout, "%04d_%s  %s\n", s.Version, s.Name, state)
		}
		if err != nil {
			fmt.Fprintf(stderr, "otter migrate: %v\n", err)
			return 1
		}
		return 0
	}

	done, err := migrate.Apply(ctx, conn, ms)
	for _, m := range done {
		fmt.Fprintf(stdout, "applied %04d_%s\n", m.Version, m.Name)
	}
	if err != nil {
		fmt.Fprintf(stderr, "otter migrate: %v\n", err)
		return 1
	}
	if len(done) == 0 {
		fmt.Fprintln(stdout, "up to date")
	}
	return 0
}
