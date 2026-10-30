package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ottermq/otter_cloud/internal/clustertest"
	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/instances"
	"github.com/ottermq/otter_cloud/internal/operations"
	"github.com/ottermq/otter_cloud/internal/postgres"
)

// checkHealth runs one pass and returns c's result.
func (p *provisioning) checkHealth(c instances.Created) operations.Health {
	p.t.Helper()
	hs, err := p.driver.CheckHealth(context.Background(), p.worker.DB)
	if err != nil {
		p.t.Fatalf("check health: %v", err)
	}
	for _, h := range hs {
		if h.InstanceID == c.Instance.ID {
			return h
		}
	}
	return operations.Health{}
}

func (p *provisioning) health(c instances.Created) string {
	p.t.Helper()
	var h string
	if err := p.db.ConnAs(dbtest.RoleMigrator).QueryRow(context.Background(),
		`SELECT health FROM instances WHERE id = $1`, c.Instance.ID).Scan(&h); err != nil {
		p.t.Fatal(err)
	}
	return h
}

// The acceptance of #97: a database that refuses connections is degraded
// at the next check, and ok again at the one after it takes them; each
// change is reported once.
func TestHealthRefusesConnections(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("production")
	if h := p.checkHealth(c); !h.Up || !h.Changed || p.health(c) != "ok" {
		t.Fatalf("first check: %+v, health %s", h, p.health(c))
	}
	if h := p.checkHealth(c); !h.Up || h.Changed {
		t.Errorf("second check: %+v, want up and unchanged", h)
	}

	p.adminExec("ALTER DATABASE " + c.Instance.InfraName + " ALLOW_CONNECTIONS false")
	h := p.checkHealth(c)
	if h.Up || !h.Changed || !strings.Contains(h.Detail, "not currently accepting connections") || p.health(c) != "degraded" {
		t.Errorf("refusing: %+v, health %s", h, p.health(c))
	}
	if h := p.checkHealth(c); h.Up || h.Changed {
		t.Errorf("still refusing: %+v, want down and unchanged", h)
	}
	if s, _ := p.instance(c); s != "running" {
		t.Errorf("status %s: health is not the lifecycle", s)
	}

	p.adminExec("ALTER DATABASE " + c.Instance.InfraName + " ALLOW_CONNECTIONS true")
	if h := p.checkHealth(c); !h.Up || !h.Changed || p.health(c) != "ok" {
		t.Errorf("taking them again: %+v, health %s", h, p.health(c))
	}
}

// PgBouncer is half the way in: a database it doesn't serve is degraded.
func TestHealthWithoutAPoolEntry(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("production")
	if _, err := postgres.EnsurePoolEntries(context.Background(), p.JailDeck().Client(), postgres.Pool{Jail: clustertest.Jail}, nil); err != nil {
		t.Fatal(err)
	}
	if h := p.checkHealth(c); h.Up || h.Detail != "PgBouncer: no entry" || p.health(c) != "degraded" {
		t.Errorf("without its entry: %+v, health %s", h, p.health(c))
	}
}

// A stopped database refuses connections by design: its health is
// unknown, and it isn't checked.
func TestHealthOfAStoppedDatabase(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("production")
	p.checkHealth(c)
	p.request("stop", c)
	p.runOnce()
	if h := p.checkHealth(c); h.InstanceID != "" || p.health(c) != "unknown" {
		t.Errorf("stopped: %+v, health %s", h, p.health(c))
	}
}

// A cluster the worker can't reach can't be reached by customers either.
func TestHealthUnreachableCluster(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("production")
	p.driver.Port = 1
	if h := p.checkHealth(c); h.Up || p.health(c) != "degraded" {
		t.Errorf("unreachable: %+v, health %s", h, p.health(c))
	}
}
