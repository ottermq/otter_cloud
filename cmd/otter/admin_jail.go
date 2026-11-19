package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ottermq/otter_cloud/deploy"
	"github.com/ottermq/otter_cloud/internal/config"
	"github.com/ottermq/otter_cloud/internal/instances"
	"github.com/ottermq/otter_cloud/internal/jaildeck"
	"github.com/ottermq/otter_cloud/internal/platformdb"
	"github.com/ottermq/otter_cloud/internal/postgres"
)

// The roles otter admin jail reclone knows, from the jail's otter:role
// property.
const (
	roleCluster    = "cluster"
	rolePlatformDB = "platform-db"
	roleAPI        = "api"
)

// etcDir is the jail's etc dataset (#216): what a reclone keeps.
const etcDir = "/usr/local/etc/otter"

// needOnEtc is what each role must have on its etc dataset before its root
// is replaced: without them it would come back unconfigured.
var needOnEtc = map[string][]string{
	roleCluster:    {"pg-bootstrap.args", "pgbouncer.users", "pgbouncer.pgpass", "pgbouncer.d/otter-databases.ini"},
	rolePlatformDB: {"platform-db-bootstrap.args", "bootstrap.sql"},
	roleAPI:        {"api.env", "db-password", "platform-db.crt"},
}

// mayBeEmpty are the files of needOnEtc that need only exist: a cluster
// with no databases routes none.
var mayBeEmpty = map[string]bool{"pgbouncer.d/otter-databases.ini": true}

// jailDeck is what a reclone uses of JailDeck.
type jailDeck interface {
	Jail(ctx context.Context, name string) (jaildeck.Jail, error)
	Template(ctx context.Context, name string) (jaildeck.Template, error)
	Exec(ctx context.Context, jail string, req jaildeck.ExecRequest) (jaildeck.ExecResult, error)
	Reclone(ctx context.Context, jail, snapshot string) (jaildeck.Reclone, error)
}

// recloner moves one of Otter's jails onto a refreshed template (#216):
// check, reclone, put back what the root held, check again. It stops at
// the first failure and says where things are.
type recloner struct {
	jd  jailDeck
	out io.Writer
	// copyBinary puts this otter into the api jail; it is too large for an
	// exec argument.
	copyBinary func(ctx context.Context, jail string) error
	// checkPool makes the cluster's PgBouncer route what the platform DB
	// says and checks it took (postgres.EnsurePoolEntries).
	checkPool func(ctx context.Context, jail string) error
	// checkPlatformDB logs in to the platform DB as this command's role.
	checkPlatformDB func(ctx context.Context) error
	// checkAPI asks the api's /healthz at the jail's address.
	checkAPI func(ctx context.Context, ip string) error
}

func (r *recloner) say(format string, args ...any) { fmt.Fprintf(r.out, format+"\n", args...) }

func (r *recloner) run(ctx context.Context, name, template string, yes bool) error {
	j, err := r.jd.Jail(ctx, name)
	if err != nil {
		return err
	}
	role := j.Properties["otter:role"]
	if _, ok := needOnEtc[role]; !ok {
		return fmt.Errorf("%s has otter:role %q; only a cluster, platform-db or api jail is Otter's to reclone", name, role)
	}
	t, err := r.jd.Template(ctx, template)
	if err != nil {
		return fmt.Errorf("template %s: %w", template, err)
	}
	if t.State != "ready" || t.Snapshot == "" {
		return fmt.Errorf("template %s is %s, not ready", template, t.State)
	}
	if t.SupersededBy != "" {
		return fmt.Errorf("template %s is superseded by %s", template, t.SupersededBy)
	}

	// The etc dataset is mounted and holds what the root won't.
	check := []string{`[ "$(stat -f %d ` + etcDir + `)" != "$(stat -f %d /)" ] || { echo "` + etcDir + ` is not a dataset of its own"; exit 1; }`}
	for _, f := range needOnEtc[role] {
		test := "-s"
		if mayBeEmpty[f] {
			test = "-f"
		}
		check = append(check, `[ `+test+` `+etcDir+`/`+f+` ] || { echo "no `+etcDir+`/`+f+`"; exit 1; }`)
	}
	if err := r.exec(ctx, name, "sh", "-c", strings.Join(check, "\n")); err != nil {
		return fmt.Errorf("%s isn't ready for a reclone (docs/runbook-m0.md, \"Moving a jail's own files onto `etc`\"): %w", name, err)
	}

	r.say("%s (%s): reclone onto %s", name, role, t.Snapshot)
	r.say("  its root is replaced; its datasets are kept, %s among them", etcDir)
	r.say("  it is stopped for it: %s", map[string]string{
		roleCluster:    "its databases are unreachable until PgBouncer is back",
		rolePlatformDB: "the portal and the worker lose the platform DB until it is back",
		roleAPI:        "the portal is down until the api is back",
	}[role])
	if !yes {
		r.say("dry run: nothing changed; run again with -yes")
		return nil
	}

	res, err := r.jd.Reclone(ctx, name, t.Snapshot)
	if err != nil {
		r.say("reclone failed at %q; JailDeck left the jail stopped", res.FailedAt)
		for _, p := range res.Parked {
			r.say("  %s is now at %s", p.Was, p.Now)
		}
		return err
	}
	r.say("recloned from %s; the old root is kept at %s", res.From, res.OldRoot)

	if err := r.converge(ctx, name, role); err != nil {
		return fmt.Errorf("recloned, but putting back its configuration failed: %w; the old root is at %s", err, res.OldRoot)
	}
	if err := r.verify(ctx, name, role, j.IP); err != nil {
		return fmt.Errorf("recloned and configured, but the check failed: %w; the old root is at %s", err, res.OldRoot)
	}
	r.say("done: %s runs on %s. The old root stays at %s until JailDeck's operator removes it.", name, template, res.OldRoot)
	return nil
}

// converge puts back what the jail's root held and the template doesn't:
// scripts, rc.conf lines, users. Each step is safe to run again.
func (r *recloner) converge(ctx context.Context, name, role string) error {
	switch role {
	case roleCluster:
		r.say("  the cluster bootstrap, with its saved arguments")
		if err := r.put(ctx, name, "/usr/local/sbin/otter-pg-bootstrap", "0555", deploy.PGBootstrap); err != nil {
			return err
		}
		return r.exec(ctx, name, "sh", "-c", "exec /usr/local/sbin/otter-pg-bootstrap $(cat "+etcDir+"/pg-bootstrap.args) all")
	case rolePlatformDB:
		r.say("  the platform DB bootstrap, with its saved arguments, and its nightly dump")
		if err := r.put(ctx, name, "/usr/local/sbin/otter-platform-db-bootstrap", "0555", deploy.PlatformDBBootstrap); err != nil {
			return err
		}
		if err := r.put(ctx, name, "/usr/local/etc/periodic/daily/600.otter-platform-dump", "0555", deploy.PlatformDump); err != nil {
			return err
		}
		if err := r.exec(ctx, name, "sysrc", "-f", "/etc/periodic.conf", "daily_otter_platform_dump_enable=YES"); err != nil {
			return err
		}
		return r.exec(ctx, name, "sh", "-c", "exec /usr/local/sbin/otter-platform-db-bootstrap $(cat "+etcDir+"/platform-db-bootstrap.args)")
	case roleAPI:
		r.say("  the otter user (1001), this otter binary and its rc script; then the api starts")
		if err := r.exec(ctx, name, "sh", "-c", `pw groupshow otter >/dev/null 2>&1 || pw groupadd otter -g 1001
id otter >/dev/null 2>&1 || pw useradd otter -u 1001 -g otter -d /nonexistent -s /usr/sbin/nologin -c "Otter API"
[ "$(id -u otter)" = 1001 ] || { echo "otter is uid $(id -u otter), not 1001: the files on etc are 1001's"; exit 1; }`); err != nil {
			return err
		}
		if err := r.put(ctx, name, "/usr/local/etc/rc.d/otter_api", "0555", deploy.APIRCScript); err != nil {
			return err
		}
		if err := r.copyBinary(ctx, name); err != nil {
			return fmt.Errorf("copy the otter binary: %w", err)
		}
		if err := r.exec(ctx, name, "sysrc", "otter_api_enable=YES"); err != nil {
			return err
		}
		return r.exec(ctx, name, "sh", "-c", "service otter_api status >/dev/null 2>&1 && service otter_api restart || service otter_api start")
	}
	return nil
}

func (r *recloner) verify(ctx context.Context, name, role, ip string) error {
	switch role {
	case roleCluster:
		r.say("  check: PgBouncer routes exactly what the platform DB says")
		return r.checkPool(ctx, name)
	case rolePlatformDB:
		r.say("  check: a login to the platform DB")
		return r.checkPlatformDB(ctx)
	case roleAPI:
		r.say("  check: the api's /healthz")
		return r.checkAPI(ctx, ip)
	}
	return nil
}

// exec runs argv in the jail and fails unless it exits 0.
func (r *recloner) exec(ctx context.Context, jail string, argv ...string) error {
	res, err := r.jd.Exec(ctx, jail, jaildeck.ExecRequest{Argv: argv, Timeout: 3 * time.Minute})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 || res.TimedOut {
		msg := strings.TrimSpace(res.Stderr + "\n" + res.Stdout)
		if len(msg) > 500 {
			msg = "…" + msg[len(msg)-500:]
		}
		return fmt.Errorf("%s: exit %d: %s", argv[0], res.ExitCode, msg)
	}
	return nil
}

// put writes content to path in the jail, whole: a new file, then a rename.
func (r *recloner) put(ctx context.Context, jail, path, mode, content string) error {
	return r.exec(ctx, jail, "sh", "-c",
		`umask 022; mkdir -p "$(dirname "$1")" && printf %s "$3" > "$1.new" && chmod "$2" "$1.new" && mv -f "$1.new" "$1"`,
		"_", path, mode, content)
}

func runJailReclone(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("admin jail reclone", "Move one of Otter's jails onto a refreshed template (#216)", stderr)
	yes := fs.Bool("yes", false, "reclone; without it, only check and say what would happen")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: otter admin jail reclone [-yes] <jail> <template>")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Run as root on the node. OTTER_JAILDECK_TOKEN_FILE must hold a token that may reclone and exec in the jail;")
		fmt.Fprintln(stderr, "the worker's may not. The platform DB settings are the other admin commands'.")
		fs.PrintDefaults()
	}
	// Not parse: it refuses positional arguments, and this takes two.
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	cfg, ok := loadConfig("admin", config.Needs{Database: true, JailDeck: true}, stderr)
	if !ok {
		return 1
	}
	ctx := context.Background()
	jd := jaildeck.New(cfg.JailDeckSocket, cfg.JailDeckToken.Reveal())
	r := &recloner{jd: jd, out: stdout,
		copyBinary: copyOwnBinary,
		checkPool: func(ctx context.Context, jail string) error {
			conn, err := platformdb.Connect(ctx, cfg)
			if err != nil {
				return err
			}
			defer conn.Close(ctx)
			clusters, err := instances.ListClusters(ctx, conn)
			if err != nil {
				return err
			}
			for _, c := range clusters {
				if c.JailName != jail {
					continue
				}
				entries, err := postgres.PoolEntries(ctx, conn, c.ID)
				if err != nil {
					return err
				}
				if _, err := postgres.EnsurePoolEntries(ctx, jd, postgres.Pool{Jail: jail}, entries); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "  PgBouncer routes the cluster's %d databases\n", len(entries))
				return nil
			}
			return fmt.Errorf("no cluster in the platform DB has jail %s", jail)
		},
		checkPlatformDB: func(ctx context.Context) error {
			return retry(ctx, time.Minute, func() error {
				conn, err := platformdb.Connect(ctx, cfg)
				if err != nil {
					return err
				}
				defer conn.Close(ctx)
				return conn.Ping(ctx)
			})
		},
		checkAPI: func(ctx context.Context, ip string) error {
			host, _, _ := strings.Cut(ip, "/")
			return retry(ctx, time.Minute, func() error {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, "8080")+"/healthz", nil)
				if err != nil {
					return err
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					return err
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					return fmt.Errorf("/healthz: %s", resp.Status)
				}
				return nil
			})
		},
	}
	if err := r.run(ctx, fs.Arg(0), fs.Arg(1), *yes); err != nil {
		fmt.Fprintf(stderr, "otter admin jail reclone: %v\n", err)
		return 1
	}
	return 0
}

// retry calls f until it succeeds or within has passed: a service just
// started may take a moment to answer.
func retry(ctx context.Context, within time.Duration, f func() error) error {
	deadline := time.Now().Add(within)
	for {
		err := f()
		if err == nil || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

// copyOwnBinary puts this otter into the jail through jexec, on stdin, so
// the jail's own view of its paths decides where it lands: a symlink in
// the jail can't redirect the write onto the host (deploy/control-plane).
func copyOwnBinary(ctx context.Context, jail string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	f, err := os.Open(self)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := exec.CommandContext(ctx, "jexec", jail, "sh", "-c",
		"umask 022 && mkdir -p /usr/local/bin && cat > /usr/local/bin/otter.new && chmod 0555 /usr/local/bin/otter.new && mv -f /usr/local/bin/otter.new /usr/local/bin/otter")
	cmd.Stdin = f
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
