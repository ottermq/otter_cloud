package instances

import (
	"context"
	"fmt"
)

// Allocation is what one instance holds, from its flavor.
type Allocation struct {
	StorageGB   int
	Connections int
}

// clusterUsage is a cluster's configured capacity and what is placed on it:
// every instance that isn't deleted, failed ones included, because they may
// still hold data (domain-model.md §4).
type clusterUsage struct {
	Cluster
	UsedStorageGB   int
	UsedConnections int
	UsedDatabases   int
}

func (u clusterUsage) fits(a Allocation) bool {
	return u.UsedStorageGB+a.StorageGB <= u.CapacityStorageGB &&
		u.UsedConnections+a.Connections <= u.CapacityConnections &&
		u.UsedDatabases+1 <= u.CapacityDatabases
}

// pick is the placement rule for a shared database (architecture.md §8):
// among the clusters it fits on, the one with the most free storage after
// the allocation; ties go to the lowest id. Storage is not overcommitted,
// and a cluster without connection headroom or room for another database
// is not a candidate.
func pick(cs []clusterUsage, a Allocation) (clusterUsage, bool) {
	var (
		best  clusterUsage
		found bool
	)
	for _, c := range cs {
		if !c.fits(a) {
			continue
		}
		free := c.CapacityStorageGB - c.UsedStorageGB - a.StorageGB
		bestFree := best.CapacityStorageGB - best.UsedStorageGB - a.StorageGB
		if !found || free > bestFree || free == bestFree && c.ID < best.ID {
			best, found = c, true
		}
	}
	return best, found
}

// placeShared chooses a cluster for a shared database of allocation a,
// inside the create transaction. It locks every eligible cluster first, in
// id order, so concurrent creates serialise on the clusters they could
// share and no two can each see the last free slot. The locks are
// transaction-scoped advisory locks: otter api reads clusters but may not
// update them, which FOR UPDATE would need.
func placeShared(ctx context.Context, db DB, a Allocation) (clusterUsage, bool, error) {
	eligible, err := EligibleClusters(ctx, db, "postgresql")
	if err != nil {
		return clusterUsage{}, false, fmt.Errorf("list clusters: %w", err)
	}
	for _, c := range eligible {
		if _, err := db.Exec(ctx, `SELECT pg_advisory_xact_lock($1::int4, hashtext($2))`, clusterLockClass, c.ID); err != nil {
			return clusterUsage{}, false, fmt.Errorf("lock cluster %s: %w", c.Name, err)
		}
	}
	usage := make([]clusterUsage, 0, len(eligible))
	for _, c := range eligible {
		u := clusterUsage{Cluster: c}
		if err := db.QueryRow(ctx, `
			SELECT coalesce(sum(storage_gb), 0), coalesce(sum(max_connections), 0), count(*)
			FROM instances WHERE cluster_id = $1 AND status <> 'deleted'`, c.ID).
			Scan(&u.UsedStorageGB, &u.UsedConnections, &u.UsedDatabases); err != nil {
			return clusterUsage{}, false, fmt.Errorf("sum cluster %s: %w", c.Name, err)
		}
		usage = append(usage, u)
	}
	u, ok := pick(usage, a)
	return u, ok, nil
}

// clusterLockClass is the first key of a cluster's advisory lock, so it
// can't collide with other advisory locks (the migration runner's, later
// ones).
const clusterLockClass = 0x6f74_6301 // "otc" 1
