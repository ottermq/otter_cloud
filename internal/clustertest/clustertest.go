// Package clustertest is the cluster scripts/test-cluster.sh starts, for
// tests: a PostgreSQL and PgBouncer set up as in a cluster jail, at the
// paths a node has, and a fake JailDeck whose execs run in its container.
package clustertest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ottermq/otter_cloud/internal/jaildeck"
	"github.com/ottermq/otter_cloud/internal/jaildeck/jaildecktest"
	"github.com/ottermq/otter_cloud/internal/postgres"
)

// Jail is the cluster jail's name, as the fake JailDeck knows it.
const Jail = "otc-pg-test"

// PGMajor is the test cluster's PostgreSQL major version.
const PGMajor = 18

// Cluster is the running test cluster, named by OTTER_TEST_CLUSTER_DIR.
type Cluster struct {
	t   *testing.T
	dir string
}

// New is the test cluster. Without OTTER_TEST_CLUSTER_DIR the test is
// skipped, except in CI, where it fails.
func New(t *testing.T) Cluster {
	t.Helper()
	dir := os.Getenv("OTTER_TEST_CLUSTER_DIR")
	if dir == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("OTTER_TEST_CLUSTER_DIR is not set in CI")
		}
		t.Skip("OTTER_TEST_CLUSTER_DIR is not set; run scripts/test-cluster.sh and export it")
	}
	return Cluster{t, dir}
}

// File is one of the files the script wrote.
func (c Cluster) File(name string) []byte {
	c.t.Helper()
	b, err := os.ReadFile(filepath.Join(c.dir, name))
	if err != nil {
		c.t.Fatal(err)
	}
	return b
}

func (c Cluster) line(name string) string { return strings.TrimSpace(string(c.File(name))) }

// Host is where the cluster's listeners are published.
func (c Cluster) Host() string { return c.line("host") }

// Port is PostgreSQL's published port; PoolPort PgBouncer's.
func (c Cluster) Port() int     { n, _ := strconv.Atoi(c.line("port")); return n }
func (c Cluster) PoolPort() int { n, _ := strconv.Atoi(c.line("pool-port")); return n }

// Target is how to reach the cluster as its admin.
func (c Cluster) Target() postgres.Cluster {
	return postgres.Cluster{Host: c.Host(), Port: c.Port(), ServerCert: c.File("server.crt")}
}

// Creds are the client certificate and key the script made under name:
// "admin" is the node's.
func (c Cluster) Creds(name string) postgres.Credentials {
	c.t.Helper()
	cr, err := postgres.LoadCredentials(c.File(name+".crt"), c.File(name+".key"))
	if err != nil {
		c.t.Fatal(err)
	}
	return cr
}

// JailDeck is a fake JailDeck with the cluster jail, whose execs run, as
// root, in the test cluster's container: the stand-in for jexec into a
// cluster jail.
func (c Cluster) JailDeck() *jaildecktest.Server {
	c.t.Helper()
	container := c.line("container")
	jd := jaildecktest.New(c.t, jaildeck.Jail{Name: Jail, Status: "running"})
	jd.OnExec(func(_ string, argv []string, env map[string]string, timeout time.Duration) jaildeck.ExecResult {
		args := []string{"exec", "-u", "root"}
		for k, v := range env {
			args = append(args, "-e", k+"="+v)
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "docker", append(append(args, container), argv...)...)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		r := jaildeck.ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
		var exit *exec.ExitError
		switch {
		case ctx.Err() != nil:
			r.TimedOut = true
		case errors.As(err, &exit):
			r.ExitCode = exit.ExitCode()
		case err != nil:
			r.ExitCode, r.Stderr = 127, err.Error()
		}
		return r
	})
	return jd
}

// Pool is the cluster's PgBouncer, at the node's paths, with its databases
// file emptied before and after the test: the cluster outlives the tests.
func (c Cluster) Pool(jd *jaildeck.Client) postgres.Pool {
	c.t.Helper()
	p := postgres.Pool{Jail: Jail}
	reset := func() {
		if _, err := postgres.EnsurePoolEntries(context.Background(), jd, p, nil); err != nil {
			c.t.Fatalf("reset the pool: %v", err)
		}
	}
	reset()
	c.t.Cleanup(reset)
	return p
}
