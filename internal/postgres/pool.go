package postgres

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/jaildeck"
)

// PoolEntry is one customer database's line in PgBouncer's [databases].
type PoolEntry struct {
	Name           string // otc_<12 hex>: database, role and entry
	MaxConnections int    // client connections, the flavor's
	PoolMode       string // transaction (Q23) or session
}

// Pool is a cluster's PgBouncer, reached through JailDeck exec in the
// cluster jail. The main pgbouncer.ini is the bootstrap's and %includes
// File; the worker writes only File, which holds only [databases], so no
// render can touch the listener, TLS or authentication.
type Pool struct {
	Jail string
	File string // DatabasesFile when empty
	// PassFile is the .pgpass, in the jail and readable only by root,
	// with the admin console's password: psql reads it there, and the
	// worker never sees it.
	PassFile  string // "/root/.pgpass" when empty
	AdminUser string // "pgbouncer_admin" when empty
	Port      int    // 6432 when zero
}

// DatabasesFile is where the worker's [databases] file lives in a cluster
// jail.
const DatabasesFile = "/usr/local/etc/pgbouncer.d/otter-databases.ini"

const header = "; Written by otter worker from the platform database; edits are overwritten.\n[databases]\n"

var (
	entryLine = regexp.MustCompile(`^(otc_[0-9a-f]{12}) = host=127\.0\.0\.1 port=5432 dbname=(otc_[0-9a-f]{12}) max_db_connections=([0-9]+) max_db_client_connections=([0-9]+) pool_mode=(transaction|session)$`)

	// ErrPoolRender means a render failed its own check and was not
	// written: the working file stays.
	ErrPoolRender = errors.New("the PgBouncer render failed its check")
	// ErrPoolReload means PgBouncer refused the new file, or did not end
	// up with exactly its entries; the previous file was put back.
	ErrPoolReload = errors.New("PgBouncer did not take the new databases file; the previous one was restored")
)

// RenderDatabases renders the [databases] file for entries, sorted by
// name so the same entries always give the same bytes, and checks it.
func RenderDatabases(entries []PoolEntry) (string, error) {
	es := slices.Clone(entries)
	slices.SortFunc(es, func(a, b PoolEntry) int { return strings.Compare(a.Name, b.Name) })
	var b strings.Builder
	b.WriteString(header)
	for _, e := range es {
		fmt.Fprintf(&b, "%s = host=127.0.0.1 port=5432 dbname=%s max_db_connections=%d max_db_client_connections=%d pool_mode=%s\n",
			e.Name, e.Name, ServerPool, e.MaxConnections, e.PoolMode)
	}
	s := b.String()
	if _, err := parseDatabases(s); err != nil {
		return "", err
	}
	return s, nil
}

// parseDatabases checks a render line by line against the one grammar the
// worker writes, and returns its entries.
func parseDatabases(s string) (map[string]PoolEntry, error) {
	body, ok := strings.CutPrefix(s, header)
	if !ok {
		return nil, fmt.Errorf("%w: no header", ErrPoolRender)
	}
	out := map[string]PoolEntry{}
	for line := range strings.Lines(body) {
		m := entryLine.FindStringSubmatch(strings.TrimSuffix(line, "\n"))
		if m == nil || m[1] != m[2] {
			return nil, fmt.Errorf("%w: line %q", ErrPoolRender, line)
		}
		// PgBouncer takes the last of two lines with one name, silently.
		if _, dup := out[m[1]]; dup {
			return nil, fmt.Errorf("%w: %s listed twice", ErrPoolRender, m[1])
		}
		server, _ := strconv.Atoi(m[3])
		client, _ := strconv.Atoi(m[4])
		if server != ServerPool || client < 1 || client > 10000 {
			return nil, fmt.Errorf("%w: %s: connections %d/%d", ErrPoolRender, m[1], server, client)
		}
		out[m[1]] = PoolEntry{Name: m[1], MaxConnections: client, PoolMode: m[5]}
	}
	return out, nil
}

// PoolDB is what PoolEntries reads from: the platform DB, as the worker.
type PoolDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// PoolEntries are the entries cluster's PgBouncer should have: every
// instance on it that should be running, which leaves out the stopped and
// the deleted, and those on their way there. The pool mode is transaction
// until a per-database option exists (Q23).
func PoolEntries(ctx context.Context, db PoolDB, clusterID string) ([]PoolEntry, error) {
	rows, err := db.Query(ctx, `
		SELECT infra_name, max_connections FROM instances
		WHERE cluster_id = $1 AND status <> 'deleted' AND desired_state = 'running'
		ORDER BY infra_name`, clusterID)
	if err != nil {
		return nil, fmt.Errorf("list the cluster's databases: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PoolEntry, error) {
		e := PoolEntry{PoolMode: "transaction"}
		return e, r.Scan(&e.Name, &e.MaxConnections)
	})
}

// EnsurePoolEntries makes the cluster's PgBouncer serve exactly entries.
// When the file already says so and PgBouncer agrees, it changes nothing.
// Otherwise it writes the file (the old one kept as .prev), reloads, and
// checks SHOW DATABASES; if PgBouncer refuses the file or ends up with
// anything else, it puts the previous file back, reloads again, and
// returns ErrPoolReload. It reports whether it wrote.
func EnsurePoolEntries(ctx context.Context, jd *jaildeck.Client, p Pool, entries []PoolEntry) (bool, error) {
	p = p.defaults()
	want, err := RenderDatabases(entries)
	if err != nil {
		return false, err
	}
	wantEntries, _ := parseDatabases(want)

	current, err := p.run(ctx, jd, nil, "sh", "-c", `[ ! -f "$1" ] || cat "$1"`, "sh", p.File)
	if err != nil {
		return false, err
	}
	if current == want {
		if live, err := p.live(ctx, jd); err == nil && sameEntries(live, wantEntries) {
			return false, nil
		}
		// The file is right but PgBouncer isn't: reload it below.
	} else if _, err := p.run(ctx, jd, nil, "sh", "-c",
		// The content is an argument, never part of the script.
		`set -e; f=$2; if [ -f "$f" ]; then cp -p "$f" "$f.prev"; else : > "$f.prev"; fi
		 printf '%s' "$1" > "$f.tmp"; chmod 0644 "$f.tmp"; mv "$f.tmp" "$f"`,
		"sh", want, p.File); err != nil {
		return false, fmt.Errorf("write %s: %w", p.File, err)
	}

	reloadErr := p.reload(ctx, jd)
	if reloadErr == nil {
		live, err := p.live(ctx, jd)
		switch {
		case err != nil:
			reloadErr = err
		case !sameEntries(live, wantEntries):
			reloadErr = fmt.Errorf("SHOW DATABASES has %d entries, the file %d", len(live), len(wantEntries))
		default:
			return true, nil
		}
	}
	if current == want {
		return false, fmt.Errorf("reload PgBouncer: %w", reloadErr)
	}
	if _, err := p.run(ctx, jd, nil, "sh", "-c", `mv "$1.prev" "$1"`, "sh", p.File); err != nil {
		return true, fmt.Errorf("%w (%v); restoring it failed too: %v", ErrPoolReload, reloadErr, err)
	}
	if err := p.reload(ctx, jd); err != nil {
		return true, fmt.Errorf("%w (%v); reloading it failed too: %v", ErrPoolReload, reloadErr, err)
	}
	return true, fmt.Errorf("%w: %v", ErrPoolReload, reloadErr)
}

func (p Pool) defaults() Pool {
	if p.File == "" {
		p.File = DatabasesFile
	}
	if p.PassFile == "" {
		p.PassFile = "/root/.pgpass"
	}
	if p.AdminUser == "" {
		p.AdminUser = "pgbouncer_admin"
	}
	if p.Port == 0 {
		p.Port = 6432
	}
	return p
}

// console runs one admin-console command with psql in the jail.
func (p Pool) console(ctx context.Context, jd *jaildeck.Client, command string) (string, error) {
	return p.run(ctx, jd, map[string]string{"PGPASSFILE": p.PassFile, "PGSSLMODE": "require", "PGCONNECT_TIMEOUT": "5"},
		"psql", "-X", "-q", "-A", "-F", "|", "-v", "ON_ERROR_STOP=1",
		"-h", "127.0.0.1", "-p", strconv.Itoa(p.Port), "-U", p.AdminUser, "-d", "pgbouncer", "-c", command)
}

func (p Pool) reload(ctx context.Context, jd *jaildeck.Client) error {
	_, err := p.console(ctx, jd, "RELOAD")
	return err
}

// live is what PgBouncer serves now: its otc_ entries.
func (p Pool) live(ctx context.Context, jd *jaildeck.Client) (map[string]PoolEntry, error) {
	out, err := p.console(ctx, jd, "SHOW DATABASES")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	col := map[string]int{}
	for i, name := range strings.Split(lines[0], "|") {
		col[name] = i
	}
	for _, c := range []string{"name", "database", "pool_mode", "max_connections", "max_client_connections"} {
		if _, ok := col[c]; !ok {
			return nil, fmt.Errorf("SHOW DATABASES has no %s column", c)
		}
	}
	live := map[string]PoolEntry{}
	for _, line := range lines[1:] {
		f := strings.Split(line, "|")
		if len(f) != len(col) || !namePattern.MatchString(f[col["name"]]) {
			continue // the console's own entry, and psql's row count
		}
		server, _ := strconv.Atoi(f[col["max_connections"]])
		client, _ := strconv.Atoi(f[col["max_client_connections"]])
		if f[col["database"]] != f[col["name"]] || server != ServerPool {
			return nil, fmt.Errorf("PgBouncer serves %s as %s with %d server connections", f[col["name"]], f[col["database"]], server)
		}
		live[f[col["name"]]] = PoolEntry{Name: f[col["name"]], MaxConnections: client, PoolMode: f[col["pool_mode"]]}
	}
	return live, nil
}

func sameEntries(a, b map[string]PoolEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func (p Pool) run(ctx context.Context, jd *jaildeck.Client, env map[string]string, argv ...string) (string, error) {
	r, err := jd.Exec(ctx, p.Jail, jaildeck.ExecRequest{Argv: argv, Env: env, Timeout: 30 * time.Second})
	if err != nil {
		return "", err
	}
	if r.ExitCode != 0 || r.TimedOut || r.StdoutTruncated {
		return "", &ExecError{Argv: argv[0], ExitCode: r.ExitCode, TimedOut: r.TimedOut, Stderr: strings.TrimSpace(r.Stderr)}
	}
	return r.Stdout, nil
}

// ExecError is a command in a cluster jail that didn't succeed.
type ExecError struct {
	Argv     string
	ExitCode int
	TimedOut bool
	Stderr   string
}

func (e *ExecError) Error() string {
	if e.TimedOut {
		return e.Argv + " timed out"
	}
	return fmt.Sprintf("%s exited %d: %s", e.Argv, e.ExitCode, e.Stderr)
}
