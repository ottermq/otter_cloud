package instances

import "testing"

func TestPick(t *testing.T) {
	c := func(id string, storage, usedStorage, conns, usedConns, dbs, usedDBs int) clusterUsage {
		return clusterUsage{
			Cluster:       Cluster{ID: id, CapacityStorageGB: storage, CapacityConnections: conns, CapacityDatabases: dbs},
			UsedStorageGB: usedStorage, UsedConnections: usedConns, UsedDatabases: usedDBs,
		}
	}
	small := Allocation{StorageGB: 2, Connections: 20}
	for _, tc := range []struct {
		name string
		cs   []clusterUsage
		want string // "" for none
	}{
		{"none", nil, ""},
		{"one that fits", []clusterUsage{c("a", 60, 0, 300, 0, 12, 0)}, "a"},
		{"most free storage wins", []clusterUsage{c("a", 60, 40, 300, 0, 12, 1), c("b", 60, 10, 300, 0, 12, 1)}, "b"},
		{"tie goes to the lowest id", []clusterUsage{c("b", 60, 10, 300, 0, 12, 1), c("a", 60, 10, 300, 0, 12, 1)}, "a"},
		{"no connection headroom", []clusterUsage{c("a", 60, 0, 300, 290, 12, 1), c("b", 60, 50, 300, 0, 12, 1)}, "b"},
		{"exactly full on connections fits", []clusterUsage{c("a", 60, 0, 300, 280, 12, 1)}, "a"},
		{"storage exactly full fits", []clusterUsage{c("a", 60, 58, 300, 0, 12, 1)}, "a"},
		{"storage over", []clusterUsage{c("a", 60, 59, 300, 0, 12, 1)}, ""},
		{"no room for another database", []clusterUsage{c("a", 60, 0, 300, 0, 3, 3), c("b", 60, 50, 300, 0, 12, 1)}, "b"},
		{"nothing fits", []clusterUsage{c("a", 60, 59, 300, 0, 12, 1), c("b", 60, 0, 300, 300, 12, 1)}, ""},
	} {
		got, ok := pick(tc.cs, small)
		if tc.want == "" {
			if ok {
				t.Errorf("%s: picked %s, want none", tc.name, got.ID)
			}
			continue
		}
		if !ok || got.ID != tc.want {
			t.Errorf("%s: picked %q (%t), want %s", tc.name, got.ID, ok, tc.want)
		}
	}
}
