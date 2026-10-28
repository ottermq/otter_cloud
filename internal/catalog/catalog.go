package catalog

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
)

// DB is what the catalog needs from a connection or a pool.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ServiceType is what an instance is. The list is code, not data: each
// type has a driver in the worker that knows how to provision it.
type ServiceType string

// PostgreSQL is a database in a shared PostgreSQL cluster (Q20).
const PostgreSQL ServiceType = "postgresql"

var serviceTypeNames = map[ServiceType]string{
	PostgreSQL: "PostgreSQL",
}

// ServiceTypes are the known service types, in display order.
var ServiceTypes = []ServiceType{PostgreSQL}

// Valid reports whether t is a known service type.
func (t ServiceType) Valid() bool {
	_, ok := serviceTypeNames[t]
	return ok
}

// Name is the service type's display name.
func (t ServiceType) Name() string { return serviceTypeNames[t] }

// Flavor is a technical size an instance can be created with. Which fields
// are set depends on the service type: a shared database has storage and
// connections, and no memory or CPU of its own.
type Flavor struct {
	Code           string      `json:"code"`
	ServiceType    ServiceType `json:"service_type"`
	Name           string      `json:"name"`
	StorageGB      int         `json:"storage_gb"`
	MaxConnections *int        `json:"max_connections,omitempty"`
	MemoryMB       *int        `json:"memory_mb,omitempty"`
	VCPU           *int        `json:"vcpu,omitempty"`
}

// flavors.json is the reviewed list of flavors (Q12): a change to it is a
// pull request. otter migrate syncs it into the flavors table.
//
//go:embed flavors.json
var flavorsFile []byte

// Seed returns the flavors in flavors.json, checked.
func Seed() ([]Flavor, error) {
	var f struct {
		Flavors []Flavor `json:"flavors"`
	}
	dec := json.NewDecoder(bytes.NewReader(flavorsFile))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("flavors.json: %w", err)
	}
	if err := check(f.Flavors); err != nil {
		return nil, fmt.Errorf("flavors.json: %w", err)
	}
	return f.Flavors, nil
}

var codePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// check applies the rules the flavors table also enforces, so a bad file
// is refused with a clear message before it reaches the database.
func check(fs []Flavor) error {
	seen := map[string]bool{}
	for _, f := range fs {
		switch {
		case !codePattern.MatchString(f.Code):
			return fmt.Errorf("flavor %q: code must be lowercase letters, digits and hyphens, 2–32 long", f.Code)
		case seen[f.Code]:
			return fmt.Errorf("flavor %s: listed twice", f.Code)
		case !f.ServiceType.Valid():
			return fmt.Errorf("flavor %s: unknown service type %q", f.Code, f.ServiceType)
		case f.Name == "" || len(f.Name) > 64:
			return fmt.Errorf("flavor %s: name must be 1–64 bytes", f.Code)
		case f.StorageGB <= 0:
			return fmt.Errorf("flavor %s: storage_gb must be positive", f.Code)
		case !positive(f.MaxConnections) || !positive(f.MemoryMB) || !positive(f.VCPU):
			return fmt.Errorf("flavor %s: sizes must be positive", f.Code)
		}
		if f.ServiceType == PostgreSQL && (f.MaxConnections == nil || f.MemoryMB != nil || f.VCPU != nil) {
			return fmt.Errorf("flavor %s: a shared database has max_connections, and no memory_mb or vcpu", f.Code)
		}
		seen[f.Code] = true
	}
	return nil
}

func positive(p *int) bool { return p == nil || *p > 0 }

// ActiveFlavors lists the flavors customers can choose, by service type
// and size.
func ActiveFlavors(ctx context.Context, db DB) ([]Flavor, error) {
	rows, err := db.Query(ctx, `
		SELECT code, service_type, name, storage_gb, max_connections, memory_mb, vcpu
		FROM flavors WHERE active
		ORDER BY service_type, storage_gb, code`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanFlavor)
}

func scanFlavor(row pgx.CollectableRow) (Flavor, error) {
	var f Flavor
	err := row.Scan(&f.Code, &f.ServiceType, &f.Name, &f.StorageGB, &f.MaxConnections, &f.MemoryMB, &f.VCPU)
	return f, err
}

// ErrSizeChanged means the file changes the sizes of a flavor that already
// exists. Instances copy their allocation from their flavor, and a code
// should mean one size for its whole life, so a new size needs a new code.
var ErrSizeChanged = errors.New("a flavor's sizes are fixed once it exists: add a new code instead, and drop this one from the file to deactivate it")

// Sync makes the flavors table match fs: it adds new flavors, renames,
// reactivates those listed again, and deactivates those no longer listed.
// It never deletes a row and never changes a flavor's sizes. It returns
// what it changed, one line each, and runs in one transaction, so a refused
// file changes nothing.
func Sync(ctx context.Context, db DB, fs []Flavor) ([]string, error) {
	if err := check(fs); err != nil {
		return nil, err
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Reads stay possible; a second sync waits.
	if _, err := tx.Exec(ctx, `LOCK TABLE flavors IN EXCLUSIVE MODE`); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		SELECT code, service_type, name, storage_gb, max_connections, memory_mb, vcpu, active
		FROM flavors`)
	if err != nil {
		return nil, err
	}
	type stored struct {
		Flavor
		active bool
	}
	existing, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (stored, error) {
		var s stored
		err := row.Scan(&s.Code, &s.ServiceType, &s.Name, &s.StorageGB, &s.MaxConnections, &s.MemoryMB, &s.VCPU, &s.active)
		return s, err
	})
	if err != nil {
		return nil, err
	}
	byCode := map[string]stored{}
	for _, s := range existing {
		byCode[s.Code] = s
	}

	var changes []string
	listed := map[string]bool{}
	for _, f := range fs {
		listed[f.Code] = true
		old, ok := byCode[f.Code]
		if !ok {
			if _, err := tx.Exec(ctx, `
				INSERT INTO flavors (code, service_type, name, storage_gb, max_connections, memory_mb, vcpu)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				f.Code, f.ServiceType, f.Name, f.StorageGB, f.MaxConnections, f.MemoryMB, f.VCPU); err != nil {
				return nil, fmt.Errorf("add flavor %s: %w", f.Code, err)
			}
			changes = append(changes, "added flavor "+f.Code)
			continue
		}
		if !sameSizes(old.Flavor, f) {
			return nil, fmt.Errorf("flavor %s: %w", f.Code, ErrSizeChanged)
		}
		if old.Name != f.Name {
			changes = append(changes, fmt.Sprintf("renamed flavor %s: %q → %q", f.Code, old.Name, f.Name))
		}
		if !old.active {
			changes = append(changes, "reactivated flavor "+f.Code)
		}
		if old.Name != f.Name || !old.active {
			if _, err := tx.Exec(ctx, `UPDATE flavors SET name = $2, active = true, updated_at = now() WHERE code = $1`,
				f.Code, f.Name); err != nil {
				return nil, fmt.Errorf("update flavor %s: %w", f.Code, err)
			}
		}
	}
	for _, s := range existing {
		if s.active && !listed[s.Code] {
			if _, err := tx.Exec(ctx, `UPDATE flavors SET active = false, updated_at = now() WHERE code = $1`, s.Code); err != nil {
				return nil, fmt.Errorf("deactivate flavor %s: %w", s.Code, err)
			}
			changes = append(changes, "deactivated flavor "+s.Code)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return changes, nil
}

func sameSizes(a, b Flavor) bool {
	return a.ServiceType == b.ServiceType && a.StorageGB == b.StorageGB &&
		sameInt(a.MaxConnections, b.MaxConnections) && sameInt(a.MemoryMB, b.MemoryMB) && sameInt(a.VCPU, b.VCPU)
}

func sameInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// ErrFlavorNotFound means no active flavor has the code.
var ErrFlavorNotFound = errors.New("no such flavor; see the catalog for the ones you can choose")

// Querier is what ActiveFlavor needs: a connection, a pool or a
// transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ActiveFlavor returns the active flavor with code.
func ActiveFlavor(ctx context.Context, db Querier, code string) (Flavor, error) {
	var f Flavor
	err := db.QueryRow(ctx, `
		SELECT code, service_type, name, storage_gb, max_connections, memory_mb, vcpu
		FROM flavors WHERE code = $1 AND active`, code).
		Scan(&f.Code, &f.ServiceType, &f.Name, &f.StorageGB, &f.MaxConnections, &f.MemoryMB, &f.VCPU)
	if errors.Is(err, pgx.ErrNoRows) {
		return Flavor{}, ErrFlavorNotFound
	}
	if err != nil {
		return Flavor{}, fmt.Errorf("look up flavor: %w", err)
	}
	return f, nil
}
