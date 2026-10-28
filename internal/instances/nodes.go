package instances

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ottermq/otter_cloud/internal/catalog"
)

// DB is what instances needs from a connection, a pool or a transaction.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Statuses of a node or a cluster. Placement only ever chooses active ones.
const (
	StatusActive   = "active"
	StatusDraining = "draining" // keeps what it has, gets nothing new
	StatusOffline  = "offline"
)

// Errors an operator can act on.
var (
	ErrNodeNotFound    = errors.New("no node with this name")
	ErrClusterNotFound = errors.New("no cluster with this name")
	ErrNameTaken       = errors.New("the name is taken")
	ErrSubnetOverlaps  = errors.New("the tenant subnet overlaps another node's")
	ErrAddressTaken    = errors.New("another cluster already uses this pool address")
	ErrJailTaken       = errors.New("another cluster already uses this jail name")
)

var (
	namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	jailPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)
)

// Node is a machine that runs instances (domain-model.md InfrastructureNode).
// Capacities are configured: what the operator decided the node may hold.
type Node struct {
	ID                string
	Name              string
	Status            string
	CapacityMemoryMB  int
	CapacityVCPU      int
	CapacityStorageGB int
	TenantSubnet      netip.Prefix
	ServiceTypes      []catalog.ServiceType
	CreatedAt         time.Time
}

// AddNode records a new node, active. Run it in a transaction with the
// audit event.
func AddNode(ctx context.Context, db DB, n Node) (Node, error) {
	switch {
	case !namePattern.MatchString(n.Name):
		return Node{}, fmt.Errorf("node name %q: lowercase letters, digits and hyphens, 2–32 long", n.Name)
	case n.CapacityMemoryMB < 0 || n.CapacityVCPU < 0 || n.CapacityStorageGB < 0:
		return Node{}, errors.New("capacities must not be negative")
	case !n.TenantSubnet.IsValid() || n.TenantSubnet != n.TenantSubnet.Masked():
		return Node{}, fmt.Errorf("tenant subnet %s: give a network address, like 10.64.0.0/22", n.TenantSubnet)
	case len(n.ServiceTypes) == 0:
		return Node{}, errors.New("name at least one service type the node may host")
	}
	types := make([]string, len(n.ServiceTypes))
	for i, t := range n.ServiceTypes {
		if !t.Valid() {
			return Node{}, fmt.Errorf("unknown service type %q", t)
		}
		types[i] = string(t)
	}
	err := db.QueryRow(ctx, `
		INSERT INTO infrastructure_nodes (name, capacity_memory_mb, capacity_vcpu, capacity_storage_gb, tenant_subnet, service_types)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, status, created_at`,
		n.Name, n.CapacityMemoryMB, n.CapacityVCPU, n.CapacityStorageGB, n.TenantSubnet, types).
		Scan(&n.ID, &n.Status, &n.CreatedAt)
	if err != nil {
		return Node{}, constraintError(err, "add node")
	}
	return n, nil
}

// ListNodes lists every node, by name.
func ListNodes(ctx context.Context, db DB) ([]Node, error) {
	rows, err := db.Query(ctx, `SELECT `+nodeColumns+` FROM infrastructure_nodes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Node, error) { return scanNode(row) })
}

const nodeColumns = `id, name, status, capacity_memory_mb, capacity_vcpu, capacity_storage_gb,
	tenant_subnet, service_types, created_at`

func scanNode(row pgx.Row) (Node, error) {
	var (
		n     Node
		types []string
	)
	err := row.Scan(&n.ID, &n.Name, &n.Status, &n.CapacityMemoryMB, &n.CapacityVCPU, &n.CapacityStorageGB,
		&n.TenantSubnet, &types, &n.CreatedAt)
	for _, t := range types {
		n.ServiceTypes = append(n.ServiceTypes, catalog.ServiceType(t))
	}
	return n, err
}

// Cluster is a shared PostgreSQL cluster: one jail with PgBouncer on a node
// (domain-model.md Cluster).
type Cluster struct {
	ID                  string
	Name                string
	Node                string // the node's name
	NodeID              string
	Status              string
	PGMajor             int
	JailName            string
	PoolAddr            netip.Addr
	PoolPort            int
	MemoryMB            int // the jail's memory limit, reserved on the node
	CapacityStorageGB   int
	CapacityConnections int
	CapacityDatabases   int
	CreatedAt           time.Time
}

// ErrNodeCapacity means the node can't take the cluster: its memory or
// storage would be oversold.
var ErrNodeCapacity = errors.New("the node doesn't have the capacity")

// AddCluster records a new cluster on the node named c.Node, active. The
// node must host shared PostgreSQL, the pool address must be in its tenant
// subnet, and the node's configured memory and storage must cover all its
// clusters together. The node's row is locked, so two adds can't oversell
// it between them. Run it in a transaction with the audit event.
func AddCluster(ctx context.Context, db DB, c Cluster) (Cluster, error) {
	switch {
	case !namePattern.MatchString(c.Name):
		return Cluster{}, fmt.Errorf("cluster name %q: lowercase letters, digits and hyphens, 2–32 long", c.Name)
	case !jailPattern.MatchString(c.JailName):
		return Cluster{}, fmt.Errorf("jail name %q: lowercase letters, digits and hyphens, up to 63 long", c.JailName)
	case c.PGMajor < 13 || c.PGMajor > 99:
		return Cluster{}, fmt.Errorf("PostgreSQL major version %d is not supported", c.PGMajor)
	case !c.PoolAddr.IsValid():
		return Cluster{}, errors.New("give the pool address, PgBouncer's address in the node's tenant subnet")
	case c.PoolPort < 1 || c.PoolPort > 65535:
		return Cluster{}, fmt.Errorf("pool port %d is not a port", c.PoolPort)
	case c.MemoryMB <= 0 || c.CapacityStorageGB <= 0 || c.CapacityConnections <= 0 || c.CapacityDatabases <= 0:
		return Cluster{}, errors.New("memory and capacities must be positive")
	}
	var memory, gb int
	n, err := scanNode(db.QueryRow(ctx, `SELECT `+nodeColumns+` FROM infrastructure_nodes WHERE name = $1 FOR UPDATE`, c.Node))
	if errors.Is(err, pgx.ErrNoRows) {
		return Cluster{}, ErrNodeNotFound
	}
	if err != nil {
		return Cluster{}, fmt.Errorf("look up node: %w", err)
	}
	if !hosts(n, catalog.PostgreSQL) {
		return Cluster{}, fmt.Errorf("node %s doesn't host %s", n.Name, catalog.PostgreSQL)
	}
	if !n.TenantSubnet.Contains(c.PoolAddr) {
		return Cluster{}, fmt.Errorf("pool address %s is outside node %s's tenant subnet %s", c.PoolAddr, n.Name, n.TenantSubnet)
	}
	if err := db.QueryRow(ctx,
		`SELECT coalesce(sum(memory_mb), 0), coalesce(sum(capacity_storage_gb), 0) FROM clusters WHERE node_id = $1`,
		n.ID).Scan(&memory, &gb); err != nil {
		return Cluster{}, fmt.Errorf("sum the node's clusters: %w", err)
	}
	if memory+c.MemoryMB > n.CapacityMemoryMB {
		return Cluster{}, fmt.Errorf("%w: %d MB of memory are configured, its clusters already reserve %d MB, and this one asks %d MB",
			ErrNodeCapacity, n.CapacityMemoryMB, memory, c.MemoryMB)
	}
	if gb+c.CapacityStorageGB > n.CapacityStorageGB {
		return Cluster{}, fmt.Errorf("%w: %d GB of storage are configured, its clusters already hold %d GB, and this one asks %d GB",
			ErrNodeCapacity, n.CapacityStorageGB, gb, c.CapacityStorageGB)
	}
	c.NodeID = n.ID
	err = db.QueryRow(ctx, `
		INSERT INTO clusters (name, node_id, pg_major, jail_name, pool_addr, pool_port, memory_mb,
			capacity_storage_gb, capacity_connections, capacity_databases)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id, status, created_at`,
		c.Name, c.NodeID, c.PGMajor, c.JailName, c.PoolAddr, c.PoolPort, c.MemoryMB,
		c.CapacityStorageGB, c.CapacityConnections, c.CapacityDatabases).
		Scan(&c.ID, &c.Status, &c.CreatedAt)
	if err != nil {
		return Cluster{}, constraintError(err, "add cluster")
	}
	return c, nil
}

func hosts(n Node, t catalog.ServiceType) bool {
	for _, s := range n.ServiceTypes {
		if s == t {
			return true
		}
	}
	return false
}

// ListClusters lists every cluster, by name.
func ListClusters(ctx context.Context, db DB) ([]Cluster, error) {
	return queryClusters(ctx, db, `ORDER BY c.name`)
}

// EligibleClusters are the clusters placement may choose for a new
// database of service type t: active clusters on active nodes that host it.
// A draining or offline cluster, or one on a draining or offline node, is
// never among them. Capacity is placement's to check, under a lock.
func EligibleClusters(ctx context.Context, db DB, t catalog.ServiceType) ([]Cluster, error) {
	return queryClusters(ctx, db, `
		WHERE c.status = 'active' AND n.status = 'active' AND $1 = ANY (n.service_types)
		ORDER BY c.id`, t)
}

func queryClusters(ctx context.Context, db DB, tail string, args ...any) ([]Cluster, error) {
	rows, err := db.Query(ctx, `
		SELECT c.id, c.name, n.name, c.node_id, c.status, c.pg_major, c.jail_name, c.pool_addr, c.pool_port,
			c.memory_mb, c.capacity_storage_gb, c.capacity_connections, c.capacity_databases, c.created_at
		FROM clusters c JOIN infrastructure_nodes n ON n.id = c.node_id `+tail, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Cluster, error) {
		var c Cluster
		err := row.Scan(&c.ID, &c.Name, &c.Node, &c.NodeID, &c.Status, &c.PGMajor, &c.JailName, &c.PoolAddr, &c.PoolPort,
			&c.MemoryMB, &c.CapacityStorageGB, &c.CapacityConnections, &c.CapacityDatabases, &c.CreatedAt)
		return c, err
	})
}

// StatusChange is what SetNodeStatus or SetClusterStatus did.
type StatusChange struct {
	ID       string
	Name     string
	Previous string
	Status   string
}

// Changed reports whether the status actually changed.
func (c StatusChange) Changed() bool { return c.Previous != c.Status }

// SetNodeStatus sets the node's status. Draining keeps its instances where
// they are and stops new placements; nothing moves automatically. Run it
// in a transaction with the audit event.
func SetNodeStatus(ctx context.Context, db DB, name, status string) (StatusChange, error) {
	return setStatus(ctx, db, "infrastructure_nodes", name, status, ErrNodeNotFound)
}

// SetClusterStatus sets the cluster's status, as SetNodeStatus does.
func SetClusterStatus(ctx context.Context, db DB, name, status string) (StatusChange, error) {
	return setStatus(ctx, db, "clusters", name, status, ErrClusterNotFound)
}

func setStatus(ctx context.Context, db DB, table, name, status string, notFound error) (StatusChange, error) {
	if status != StatusActive && status != StatusDraining && status != StatusOffline {
		return StatusChange{}, fmt.Errorf("unknown status %q", status)
	}
	c := StatusChange{Name: name, Status: status}
	// table is one of two constants above, never input.
	err := db.QueryRow(ctx, `SELECT id, status FROM `+table+` WHERE name = $1 FOR UPDATE`, name).Scan(&c.ID, &c.Previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return StatusChange{}, notFound
	}
	if err != nil {
		return StatusChange{}, fmt.Errorf("look up %s: %w", name, err)
	}
	if c.Changed() {
		if _, err := db.Exec(ctx, `UPDATE `+table+` SET status = $2, updated_at = now() WHERE id = $1`, c.ID, status); err != nil {
			return StatusChange{}, fmt.Errorf("set status of %s: %w", name, err)
		}
	}
	return c, nil
}

// constraintError turns the unique and exclusion violations an operator can
// cause into errors that say what to change.
func constraintError(err error, what string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.ConstraintName {
		case "infrastructure_nodes_name_key", "clusters_name_key":
			return ErrNameTaken
		case "infrastructure_nodes_tenant_subnet_excl":
			return ErrSubnetOverlaps
		case "clusters_pool_addr_key":
			return ErrAddressTaken
		case "clusters_jail_name_key":
			return ErrJailTaken
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}

// NodeRole is the platform database login role of the node's worker: it
// is a member of otter_worker and sees only that node's rows. Keep in step
// with node_role() in 0012_worker.sql.
func NodeRole(node string) string { return "otter_node_" + strings.ReplaceAll(node, "-", "_") }
