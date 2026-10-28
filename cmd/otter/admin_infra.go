package main

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"text/tabwriter"

	"github.com/ottermq/otter_cloud/internal/audit"
	"github.com/ottermq/otter_cloud/internal/catalog"
	"github.com/ottermq/otter_cloud/internal/config"
	"github.com/ottermq/otter_cloud/internal/instances"
)

func runNodeAdd(args []string, stdout, stderr io.Writer) int {
	const name = "node add"
	fs := newFlagSet("admin "+name, "", stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: otter admin node add -memory-mb N -storage-gb N -tenant-subnet CIDR [flags] <name>")
		fmt.Fprintln(fs.Output(), "\nRecords a node, active. Capacities are what the operator decides the node may hold")
		fmt.Fprintln(fs.Output(), "for tenants: host RAM minus the host, ARC and platform reservations, not the raw RAM.")
		fmt.Fprintln(fs.Output(), "\nFlags:")
		fs.PrintDefaults()
	}
	memory := fs.Int("memory-mb", -1, "memory for tenants and cluster jails, in MB (required)")
	vcpu := fs.Int("vcpu", 0, "vCPUs for dedicated instances, overcommit included")
	storage := fs.Int("storage-gb", -1, "storage for tenants, in GB (required)")
	subnet := fs.String("tenant-subnet", "", "the node's tenant subnet, e.g. 10.64.0.0/22 (required)")
	types := fs.String("service-types", string(catalog.PostgreSQL), "comma-separated service types the node may host")
	rest, code, done := parseArgs(fs, args, 1, "a node name")
	if done {
		return code
	}
	if *memory < 0 || *storage < 0 || *subnet == "" {
		fmt.Fprintf(stderr, "otter admin %s: -memory-mb, -storage-gb and -tenant-subnet are required\n", name)
		return 2
	}
	prefix, err := netip.ParsePrefix(*subnet)
	if err != nil {
		fmt.Fprintf(stderr, "otter admin %s: -tenant-subnet: %v\n", name, err)
		return 2
	}
	n := instances.Node{Name: rest[0], CapacityMemoryMB: *memory, CapacityVCPU: *vcpu, CapacityStorageGB: *storage,
		TenantSubnet: prefix}
	for _, t := range strings.Split(*types, ",") {
		n.ServiceTypes = append(n.ServiceTypes, catalog.ServiceType(strings.TrimSpace(t)))
	}
	return infraChange(name, stdout, stderr, func(ctx context.Context, db instances.DB) (audit.Event, string, error) {
		n, err := instances.AddNode(ctx, db, n)
		if err != nil {
			return audit.Event{}, "", err
		}
		return audit.Event{Action: audit.ActionNodeAdd, TargetType: "node", TargetID: n.ID,
				Details: map[string]any{"name": n.Name, "memory_mb": n.CapacityMemoryMB, "vcpu": n.CapacityVCPU,
					"storage_gb": n.CapacityStorageGB, "tenant_subnet": n.TenantSubnet.String(), "service_types": n.ServiceTypes}},
			fmt.Sprintf("node %s added, active", n.Name), nil
	})
}

func runClusterAdd(args []string, stdout, stderr io.Writer) int {
	const name = "cluster add"
	fs := newFlagSet("admin "+name, "", stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: otter admin cluster add -node NAME -pool-addr IP -memory-mb N -storage-gb N -connections N -databases N [flags] <name>")
		fmt.Fprintln(fs.Output(), "\nRecords a shared PostgreSQL cluster that already runs on the node, active. The node's")
		fmt.Fprintln(fs.Output(), "configured memory and storage must cover all its clusters together.")
		fmt.Fprintln(fs.Output(), "\nFlags:")
		fs.PrintDefaults()
	}
	node := fs.String("node", "", "the node the cluster jail runs on (required)")
	pgMajor := fs.Int("pg-major", 18, "the cluster's PostgreSQL major version")
	jail := fs.String("jail", "", "the cluster jail's name (default otc-<name>)")
	addr := fs.String("pool-addr", "", "PgBouncer's address in the node's tenant subnet (required)")
	port := fs.Int("pool-port", 6432, "PgBouncer's port")
	memory := fs.Int("memory-mb", 0, "the cluster jail's memory limit, reserved on the node, in MB (required)")
	storage := fs.Int("storage-gb", 0, "storage the cluster may sell to databases, in GB (required)")
	conns := fs.Int("connections", 0, "client connections the cluster accepts, summed over its databases (required)")
	dbs := fs.Int("databases", 0, "most databases the cluster may hold (required)")
	rest, code, done := parseArgs(fs, args, 1, "a cluster name")
	if done {
		return code
	}
	if *node == "" || *addr == "" || *memory == 0 || *storage == 0 || *conns == 0 || *dbs == 0 {
		fmt.Fprintf(stderr, "otter admin %s: -node, -pool-addr, -memory-mb, -storage-gb, -connections and -databases are required\n", name)
		return 2
	}
	ip, err := netip.ParseAddr(*addr)
	if err != nil {
		fmt.Fprintf(stderr, "otter admin %s: -pool-addr: %v\n", name, err)
		return 2
	}
	if *jail == "" {
		*jail = "otc-" + rest[0]
	}
	c := instances.Cluster{Name: rest[0], Node: *node, PGMajor: *pgMajor, JailName: *jail, PoolAddr: ip, PoolPort: *port,
		MemoryMB: *memory, CapacityStorageGB: *storage, CapacityConnections: *conns, CapacityDatabases: *dbs}
	return infraChange(name, stdout, stderr, func(ctx context.Context, db instances.DB) (audit.Event, string, error) {
		c, err := instances.AddCluster(ctx, db, c)
		if err != nil {
			return audit.Event{}, "", err
		}
		return audit.Event{Action: audit.ActionClusterAdd, TargetType: "cluster", TargetID: c.ID,
				Details: map[string]any{"name": c.Name, "node": c.Node, "pg_major": c.PGMajor, "jail": c.JailName,
					"pool": netip.AddrPortFrom(c.PoolAddr, uint16(c.PoolPort)).String(), "memory_mb": c.MemoryMB,
					"storage_gb": c.CapacityStorageGB, "connections": c.CapacityConnections, "databases": c.CapacityDatabases}},
			fmt.Sprintf("cluster %s added on node %s, active", c.Name, c.Node), nil
	})
}

func runNodeDrain(args []string, stdout, stderr io.Writer) int {
	return setInfraStatus("node drain", "node", instances.StatusDraining, instances.SetNodeStatus, audit.ActionNodeStatus,
		"Stop placing anything new on a node. What it holds stays where it is; nothing moves automatically", args, stdout, stderr)
}

func runNodeActivate(args []string, stdout, stderr io.Writer) int {
	return setInfraStatus("node activate", "node", instances.StatusActive, instances.SetNodeStatus, audit.ActionNodeStatus,
		"Let placement use a drained or offline node again", args, stdout, stderr)
}

func runClusterDrain(args []string, stdout, stderr io.Writer) int {
	return setInfraStatus("cluster drain", "cluster", instances.StatusDraining, instances.SetClusterStatus, audit.ActionClusterStatus,
		"Stop placing new databases on a cluster. Its databases stay where they are", args, stdout, stderr)
}

func runClusterActivate(args []string, stdout, stderr io.Writer) int {
	return setInfraStatus("cluster activate", "cluster", instances.StatusActive, instances.SetClusterStatus, audit.ActionClusterStatus,
		"Let placement use a drained or offline cluster again", args, stdout, stderr)
}

func setInfraStatus(name, kind, status string,
	set func(context.Context, instances.DB, string, string) (instances.StatusChange, error),
	action, summary string, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("admin "+name, summary, stderr)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: otter admin %s [-reason text] <name>\n\n%s.\n\nFlags:\n", name, summary)
		fs.PrintDefaults()
	}
	reason := fs.String("reason", "", "why, for the audit log")
	rest, code, done := parseArgs(fs, args, 1, "a "+kind+" name")
	if done {
		return code
	}
	if code, ok := checkReason(name, *reason, stderr); !ok {
		return code
	}
	return infraChange(name, stdout, stderr, func(ctx context.Context, db instances.DB) (audit.Event, string, error) {
		c, err := set(ctx, db, rest[0], status)
		if err != nil {
			return audit.Event{}, "", err
		}
		if !c.Changed() {
			return audit.Event{}, fmt.Sprintf("%s %s is already %s; nothing changed", kind, c.Name, status), nil
		}
		details := map[string]any{"name": c.Name, "from": c.Previous, "to": c.Status}
		if *reason != "" {
			details["reason"] = *reason
		}
		return audit.Event{Action: action, TargetType: kind, TargetID: c.ID, Details: details},
			fmt.Sprintf("%s %s is %s", kind, c.Name, status), nil
	})
}

// infraChange runs change and records the audit event it returns, in one
// transaction. An event with no action means nothing changed: no audit
// row, and the message is printed as is.
func infraChange(name string, stdout, stderr io.Writer,
	change func(context.Context, instances.DB) (audit.Event, string, error)) int {
	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()
	conn, _, ok := adminConn(ctx, name, config.Needs{Database: true}, stderr)
	if !ok {
		return 1
	}
	defer conn.Close(context.Background())

	tx, err := conn.Begin(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "otter admin %s: %v\n", name, err)
		return 1
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	e, msg, err := change(ctx, tx)
	if err != nil {
		fmt.Fprintf(stderr, "otter admin %s: %v\n", name, err)
		return 1
	}
	if e.Action != "" {
		e.Actor, e.Outcome = audit.Operator(), audit.OK
		if err := audit.Record(ctx, tx, e); err != nil {
			fmt.Fprintf(stderr, "otter admin %s: %v\n", name, err)
			return 1
		}
	}
	if err := tx.Commit(ctx); err != nil {
		fmt.Fprintf(stderr, "otter admin %s: %v\n", name, err)
		return 1
	}
	fmt.Fprintln(stdout, msg)
	return 0
}

func runNodeList(args []string, stdout, stderr io.Writer) int {
	return infraList("node list", "List nodes, by name", args, stdout, stderr, func(ctx context.Context, db instances.DB, w io.Writer) error {
		nodes, err := instances.ListNodes(ctx, db)
		if err != nil {
			return err
		}
		fmt.Fprintln(w, "NAME\tSTATUS\tMEMORY\tVCPU\tSTORAGE\tTENANT SUBNET\tSERVICE TYPES")
		for _, n := range nodes {
			types := make([]string, len(n.ServiceTypes))
			for i, t := range n.ServiceTypes {
				types[i] = string(t)
			}
			fmt.Fprintf(w, "%s\t%s\t%d MB\t%d\t%d GB\t%s\t%s\n", n.Name, n.Status, n.CapacityMemoryMB, n.CapacityVCPU,
				n.CapacityStorageGB, n.TenantSubnet, strings.Join(types, ","))
		}
		return nil
	})
}

func runClusterList(args []string, stdout, stderr io.Writer) int {
	return infraList("cluster list", "List clusters, by name", args, stdout, stderr, func(ctx context.Context, db instances.DB, w io.Writer) error {
		clusters, err := instances.ListClusters(ctx, db)
		if err != nil {
			return err
		}
		fmt.Fprintln(w, "NAME\tNODE\tSTATUS\tPG\tJAIL\tPOOL\tMEMORY\tSTORAGE\tCONNECTIONS\tDATABASES")
		for _, c := range clusters {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%d MB\t%d GB\t%d\t%d\n", c.Name, c.Node, c.Status, c.PGMajor, c.JailName,
				netip.AddrPortFrom(c.PoolAddr, uint16(c.PoolPort)), c.MemoryMB, c.CapacityStorageGB,
				c.CapacityConnections, c.CapacityDatabases)
		}
		return nil
	})
}

func infraList(name, summary string, args []string, stdout, stderr io.Writer,
	list func(context.Context, instances.DB, io.Writer) error) int {
	fs := newFlagSet("admin "+name, summary, stderr)
	if code, done := parse(fs, args); done {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()
	conn, _, ok := adminConn(ctx, name, config.Needs{Database: true}, stderr)
	if !ok {
		return 1
	}
	defer conn.Close(context.Background())
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	if err := list(ctx, conn, tw); err != nil {
		fmt.Fprintf(stderr, "otter admin %s: %v\n", name, err)
		return 1
	}
	if err := tw.Flush(); err != nil {
		return 1
	}
	return 0
}
