package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ottermq/otter_cloud/deploy"
	"github.com/ottermq/otter_cloud/internal/jaildeck"
)

// fakeJailDeck records what a reclone asks of JailDeck.
type fakeJailDeck struct {
	jail     jaildeck.Jail
	template jaildeck.Template
	// failExec makes an exec whose argv contains it exit 1.
	failExec   string
	recloneErr error
	calls      []string
	execs      [][]string
}

func (f *fakeJailDeck) Jail(_ context.Context, name string) (jaildeck.Jail, error) {
	f.calls = append(f.calls, "jail "+name)
	return f.jail, nil
}

func (f *fakeJailDeck) Template(_ context.Context, name string) (jaildeck.Template, error) {
	f.calls = append(f.calls, "template "+name)
	return f.template, nil
}

func (f *fakeJailDeck) Exec(_ context.Context, jail string, req jaildeck.ExecRequest) (jaildeck.ExecResult, error) {
	f.calls = append(f.calls, "exec "+req.Argv[0])
	f.execs = append(f.execs, req.Argv)
	if f.failExec != "" && strings.Contains(strings.Join(req.Argv, " "), f.failExec) {
		return jaildeck.ExecResult{ExitCode: 1, Stdout: "refused"}, nil
	}
	return jaildeck.ExecResult{}, nil
}

func (f *fakeJailDeck) Reclone(_ context.Context, jail, snapshot string) (jaildeck.Reclone, error) {
	f.calls = append(f.calls, "reclone "+jail+" "+snapshot)
	if f.recloneErr != nil {
		return jaildeck.Reclone{FailedAt: "clone", Parked: []struct {
			Was string `json:"was"`
			Now string `json:"now"`
		}{{Was: "tank/j/" + jail + "/etc", Now: "tank/retained/etc"}}}, f.recloneErr
	}
	return jaildeck.Reclone{From: "tank/templates/pg18@base", To: snapshot, OldRoot: "tank/retained/" + jail + "-root"}, nil
}

type recloneRun struct {
	jd                          *fakeJailDeck
	out                         bytes.Buffer
	copied, pool, platform, api bool
	apiIP                       string
	r                           *recloner
}

func newRecloneRun(role string) *recloneRun {
	rr := &recloneRun{jd: &fakeJailDeck{
		jail:     jaildeck.Jail{Name: "j", IP: "10.0.0.5/24", Properties: map[string]string{"otter:role": role}},
		template: jaildeck.Template{Name: "pg18-new", Snapshot: "tank/templates/pg18-new@base", State: "ready"},
	}}
	rr.r = &recloner{jd: rr.jd, out: &rr.out,
		copyBinary:      func(context.Context, string) error { rr.copied = true; return nil },
		checkPool:       func(context.Context, string) error { rr.pool = true; return nil },
		checkPlatformDB: func(context.Context) error { rr.platform = true; return nil },
		checkAPI:        func(_ context.Context, ip string) error { rr.api, rr.apiIP = true, ip; return nil },
	}
	return rr
}

func (rr *recloneRun) recloned() bool {
	for _, c := range rr.jd.calls {
		if strings.HasPrefix(c, "reclone ") {
			return true
		}
	}
	return false
}

// ran reports whether an exec's argv contained s.
func (rr *recloneRun) ran(s string) bool {
	for _, a := range rr.jd.execs {
		if strings.Contains(strings.Join(a, "\x00"), s) {
			return true
		}
	}
	return false
}

func TestJailReclone_DryRunChangesNothing(t *testing.T) {
	rr := newRecloneRun(roleCluster)
	if err := rr.r.run(context.Background(), "j", "pg18-new", false); err != nil {
		t.Fatal(err)
	}
	if rr.recloned() || len(rr.jd.execs) != 1 {
		t.Errorf("a dry run did more than check: %v", rr.jd.calls)
	}
	if !strings.Contains(rr.out.String(), "dry run: nothing changed") {
		t.Errorf("output:\n%s", rr.out.String())
	}
	// The check covers the dataset and each file the root won't keep.
	check := strings.Join(rr.jd.execs[0], " ")
	for _, f := range append([]string{"stat -f %d " + etcDir}, needOnEtc[roleCluster]...) {
		if !strings.Contains(check, f) {
			t.Errorf("the check doesn't look at %s", f)
		}
	}
}

func TestJailReclone_Refuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*recloneRun)
		want string
	}{
		{"not Otter's", func(rr *recloneRun) { rr.jd.jail.Properties = nil }, "only a cluster"},
		{"template not ready", func(rr *recloneRun) { rr.jd.template.State = "building" }, "not ready"},
		{"template superseded", func(rr *recloneRun) { rr.jd.template.SupersededBy = "pg18-newer" }, "superseded by pg18-newer"},
		{"no etc dataset", func(rr *recloneRun) { rr.jd.failExec = "stat -f" }, "isn't ready for a reclone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := newRecloneRun(roleCluster)
			tc.set(rr)
			err := rr.r.run(context.Background(), "j", "pg18-new", true)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
			if rr.recloned() {
				t.Error("recloned anyway")
			}
		})
	}
}

func TestJailReclone_Cluster(t *testing.T) {
	rr := newRecloneRun(roleCluster)
	if err := rr.r.run(context.Background(), "j", "pg18-new", true); err != nil {
		t.Fatalf("%v\n%s", err, rr.out.String())
	}
	if !rr.recloned() || !rr.ran(deploy.PGBootstrap) || !rr.ran("$(cat "+etcDir+"/pg-bootstrap.args) all") || !rr.pool {
		t.Errorf("calls %v; pool checked %t", rr.jd.calls, rr.pool)
	}
	for _, a := range rr.jd.execs {
		if cmd := strings.Join(a, " "); strings.Contains(cmd, "pg-bootstrap.args) all") && strings.Contains(cmd, " -r ") {
			t.Errorf("the rerun rotates the passwords: %s", cmd)
		}
	}
	if !strings.Contains(rr.out.String(), "tank/retained/j-root") {
		t.Errorf("the old root isn't reported:\n%s", rr.out.String())
	}
}

func TestJailReclone_PlatformDB(t *testing.T) {
	rr := newRecloneRun(rolePlatformDB)
	if err := rr.r.run(context.Background(), "j", "pg18-new", true); err != nil {
		t.Fatal(err)
	}
	if !rr.ran(deploy.PlatformDBBootstrap) || !rr.ran(deploy.PlatformDump) ||
		!rr.ran("daily_otter_platform_dump_enable=YES") || !rr.platform {
		t.Errorf("calls %v; platform checked %t", rr.jd.calls, rr.platform)
	}
}

func TestJailReclone_API(t *testing.T) {
	rr := newRecloneRun(roleAPI)
	if err := rr.r.run(context.Background(), "j", "pg18-new", true); err != nil {
		t.Fatal(err)
	}
	if !rr.ran("pw useradd otter -u 1001") || !rr.ran(deploy.APIRCScript) || !rr.ran("otter_api_enable=YES") ||
		!rr.copied || !rr.api || rr.apiIP != "10.0.0.5/24" {
		t.Errorf("calls %v; copied %t, api checked %t at %q", rr.jd.calls, rr.copied, rr.api, rr.apiIP)
	}
}

func TestJailReclone_StopsAtTheFirstFailure(t *testing.T) {
	t.Run("the reclone", func(t *testing.T) {
		rr := newRecloneRun(roleCluster)
		rr.jd.recloneErr = errors.New("zfs said no")
		if err := rr.r.run(context.Background(), "j", "pg18-new", true); err == nil {
			t.Fatal("no error")
		}
		if rr.ran(deploy.PGBootstrap) || rr.pool {
			t.Error("converged after a failed reclone")
		}
		if !strings.Contains(rr.out.String(), "tank/j/j/etc is now at tank/retained/etc") {
			t.Errorf("where the datasets are isn't reported:\n%s", rr.out.String())
		}
	})
	t.Run("the converge", func(t *testing.T) {
		rr := newRecloneRun(roleCluster)
		rr.jd.failExec = "pg-bootstrap.args) all"
		err := rr.r.run(context.Background(), "j", "pg18-new", true)
		if err == nil || !strings.Contains(err.Error(), "tank/retained/j-root") {
			t.Errorf("err = %v, want the old root named", err)
		}
		if rr.pool {
			t.Error("checked after a failed converge")
		}
	})
}

// put's shell writes the content exactly, quotes and newlines included,
// and replaces a file whole.
func TestJailReclone_PutWritesExactly(t *testing.T) {
	var argv []string
	r := &recloner{jd: &captureExec{argv: &argv}}
	path := filepath.Join(t.TempDir(), "sub", "f")
	content := "#!/bin/sh\necho 'it''s' \"$1\" %s \\n\n"
	if err := r.put(context.Background(), "j", path, "0555", content); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != content {
		t.Fatalf("wrote %q, %v; want %q", got, err, content)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o555 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
}

type captureExec struct {
	jailDeck
	argv *[]string
}

func (c *captureExec) Exec(_ context.Context, _ string, req jaildeck.ExecRequest) (jaildeck.ExecResult, error) {
	*c.argv = req.Argv
	return jaildeck.ExecResult{}, nil
}

// The command line: two arguments, -yes before them. Past the arguments
// it needs the configuration, which this test doesn't give it.
func TestJailReclone_Args(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"-h"}, 0, "Usage: otter admin jail reclone"},
		{[]string{"otc-x"}, 2, "Usage: otter admin jail reclone"},
		{[]string{"otc-x", "pg18-new", "more"}, 2, "Usage: otter admin jail reclone"},
		{[]string{"-yes", "otc-x", "pg18-new"}, 1, "invalid configuration"},
		{[]string{"otc-x", "pg18-new"}, 1, "invalid configuration"},
	} {
		var stdout, stderr bytes.Buffer
		t.Setenv("OTTER_DATABASE_URL", "")
		t.Setenv("OTTER_JAILDECK_TOKEN_FILE", "")
		code := runAdmin(append([]string{"jail", "reclone"}, tc.args...), &stdout, &stderr)
		if code != tc.code || !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("%v: exit %d, want %d; stderr:\n%s", tc.args, code, tc.code, stderr.String())
		}
	}
}
