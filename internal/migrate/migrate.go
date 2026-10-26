// Package migrate applies the platform database's schema migrations.
//
// Migrations are numbered SQL files embedded in the binary
// (migrations/0001_name.sql, …). Each one runs in its own transaction and is
// recorded in schema_migrations with a checksum. There are no down
// migrations: a mistake is fixed by a new migration. The runner refuses to
// continue when an applied migration's file has changed, or when the
// database has a migration this binary does not know.
package migrate

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var embedded embed.FS

// lockKey is the advisory lock that keeps two runs from interleaving. Any
// fixed number works; this one spells "otter".
const lockKey int64 = 0x6f74746572

// Migration is one SQL file.
type Migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum string
}

// Status is a migration and whether it has been applied.
type Status struct {
	Migration
	AppliedAt *time.Time
}

var fileName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// Migrations returns the migrations embedded in this binary.
func Migrations() ([]Migration, error) {
	sub, err := fs.Sub(embedded, "migrations")
	if err != nil {
		return nil, err
	}
	return Load(sub)
}

// Load reads migrations from the root of fsys. Versions must start at 1 and
// have no gaps, so a missing or misnumbered file is caught at build time by
// the tests rather than at deploy time.
func Load(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var ms []Migration
	for _, e := range entries {
		m := fileName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migration file %q is not named NNNN_lower_snake.sql", e.Name())
		}
		version, _ := strconv.Atoi(m[1])
		data, err := fs.ReadFile(fsys, path.Clean(e.Name()))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		ms = append(ms, Migration{Version: version, Name: m[2], SQL: string(data), Checksum: hex.EncodeToString(sum[:])})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].Version < ms[j].Version })
	for i, m := range ms {
		if m.Version != i+1 {
			return nil, fmt.Errorf("migration versions must run 1, 2, 3… without gaps or repeats: expected %04d, found %04d_%s", i+1, m.Version, m.Name)
		}
	}
	return ms, nil
}

// Apply applies every pending migration, in order, and returns the ones it
// applied. It holds an advisory lock for the whole run, so a second runner
// waits rather than interleaving.
func Apply(ctx context.Context, conn *pgx.Conn, ms []Migration) ([]Migration, error) {
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		return nil, fmt.Errorf("take the migration lock: %w", err)
	}
	defer func() {
		// A fresh context: the lock must be released even if ctx was cancelled.
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", lockKey)
	}()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    integer     PRIMARY KEY,
		name       text        NOT NULL,
		checksum   text        NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := appliedMigrations(ctx, conn)
	if err != nil {
		return nil, err
	}
	if err := verify(ms, applied); err != nil {
		return nil, err
	}

	var done []Migration
	for _, m := range ms {
		if _, ok := applied[m.Version]; ok {
			continue
		}
		if err := applyOne(ctx, conn, m); err != nil {
			return done, err
		}
		done = append(done, m)
	}
	return done, nil
}

// List reports every known migration and when it was applied. It changes
// nothing, and works on a database that has never been migrated.
func List(ctx context.Context, conn *pgx.Conn, ms []Migration) ([]Status, error) {
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass('schema_migrations') IS NOT NULL").Scan(&exists); err != nil {
		return nil, err
	}
	applied := map[int]record{}
	if exists {
		var err error
		if applied, err = appliedMigrations(ctx, conn); err != nil {
			return nil, err
		}
	}
	out := make([]Status, 0, len(ms))
	for _, m := range ms {
		s := Status{Migration: m}
		if r, ok := applied[m.Version]; ok {
			at := r.appliedAt
			s.AppliedAt = &at
		}
		out = append(out, s)
	}
	return out, verify(ms, applied)
}

type record struct {
	name      string
	checksum  string
	appliedAt time.Time
}

func appliedMigrations(ctx context.Context, conn *pgx.Conn) (map[int]record, error) {
	rows, err := conn.Query(ctx, "SELECT version, name, checksum, applied_at FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	out := map[int]record{}
	for rows.Next() {
		var v int
		var r record
		if err := rows.Scan(&v, &r.name, &r.checksum, &r.appliedAt); err != nil {
			return nil, err
		}
		out[v] = r
	}
	return out, rows.Err()
}

// verify checks that what the database recorded matches this binary.
func verify(ms []Migration, applied map[int]record) error {
	known := map[int]Migration{}
	for _, m := range ms {
		known[m.Version] = m
	}
	var errs []error
	for v, r := range applied {
		m, ok := known[v]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("the database has migration %04d_%s, which this binary does not know: is it older than the last one deployed?", v, r.name))
		case m.Checksum != r.checksum:
			errs = append(errs, fmt.Errorf("migration %04d_%s was changed after it was applied; add a new migration instead", v, m.Name))
		}
	}
	return errors.Join(errs...)
}

func applyOne(ctx context.Context, conn *pgx.Conn, m Migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The simple protocol, so one file may hold several statements.
	if _, err := tx.Exec(ctx, m.SQL, pgx.QueryExecModeSimpleProtocol); err != nil {
		return fmt.Errorf("migration %04d_%s: %w", m.Version, m.Name, err)
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)",
		m.Version, m.Name, m.Checksum); err != nil {
		return fmt.Errorf("record migration %04d_%s: %w", m.Version, m.Name, err)
	}
	return tx.Commit(ctx)
}
