package billing

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is what billing needs from a connection, a pool or a transaction.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Limits are what an account may hold at once, summed over its instances.
// Memory and vCPU are for dedicated instances; a shared database uses
// neither.
type Limits struct {
	Instances   int `json:"max_instances"`
	StorageGB   int `json:"max_storage_gb"`
	Connections int `json:"max_connections"`
	MemoryMB    int `json:"max_memory_mb"`
	VCPU        int `json:"max_vcpu"`
}

// Plan is a commercial offering (domain-model.md Plan).
type Plan struct {
	Code       string `json:"code"`
	Name       string `json:"name"`
	PriceMinor int64  `json:"price_minor"` // in the currency's minor unit
	Currency   string `json:"currency"`
	Interval   string `json:"interval"` // "month" or "none"
	Limits
	Public bool `json:"public"` // offered for self-service; false: an operator assigns it
	Active bool `json:"-"`
}

// RequiresPayment reports whether a subscription to the plan owes money.
func (p Plan) RequiresPayment() bool { return p.PriceMinor > 0 }

// plans.json is the reviewed list of plans (Q12): a change to it is a pull
// request. otter migrate syncs it into the plans table. Plan codes appear
// nowhere else in the code: everything outside billing asks Entitlement.
//
//go:embed plans.json
var plansFile []byte

// Seed returns the plans in plans.json, checked.
func Seed() ([]Plan, error) {
	var f struct {
		Plans []Plan `json:"plans"`
	}
	dec := json.NewDecoder(bytes.NewReader(plansFile))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("plans.json: %w", err)
	}
	if err := check(f.Plans); err != nil {
		return nil, fmt.Errorf("plans.json: %w", err)
	}
	return f.Plans, nil
}

var (
	codePattern     = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
)

// check applies the rules the plans table also enforces, so a bad file is
// refused with a clear message before it reaches the database.
func check(ps []Plan) error {
	seen := map[string]bool{}
	for _, p := range ps {
		l := p.Limits
		switch {
		case !codePattern.MatchString(p.Code):
			return fmt.Errorf("plan %q: code must be lowercase letters, digits and hyphens, 2–32 long", p.Code)
		case seen[p.Code]:
			return fmt.Errorf("plan %s: listed twice", p.Code)
		case p.Name == "" || len(p.Name) > 64:
			return fmt.Errorf("plan %s: name must be 1–64 bytes", p.Code)
		case p.PriceMinor < 0:
			return fmt.Errorf("plan %s: price_minor must not be negative", p.Code)
		case !currencyPattern.MatchString(p.Currency):
			return fmt.Errorf("plan %s: currency must be an ISO 4217 code, like BRL", p.Code)
		case p.Interval != "month" && p.Interval != "none":
			return fmt.Errorf("plan %s: interval must be month or none", p.Code)
		case l.Instances < 0 || l.StorageGB < 0 || l.Connections < 0 || l.MemoryMB < 0 || l.VCPU < 0:
			return fmt.Errorf("plan %s: limits must not be negative", p.Code)
		}
		seen[p.Code] = true
	}
	return nil
}

// ErrPriceChanged means the file changes the price, currency or interval of
// a plan that already exists. Whether a plan needs payment decides how its
// subscriptions behave, so a new price is a new plan.
var ErrPriceChanged = errors.New("a plan's price, currency and interval are fixed once it exists: add a new code instead, and drop this one from the file to deactivate it")

// SyncPlans makes the plans table match ps: it adds new plans, updates the
// name, limits and visibility of existing ones, reactivates those listed
// again and deactivates those no longer listed. Changed limits apply only
// to subscriptions created afterwards (domain-model.md §2.4). It never
// deletes a plan. It returns what it changed, one line each, and runs in
// one transaction, so a refused file changes nothing.
func SyncPlans(ctx context.Context, db DB, ps []Plan) ([]string, error) {
	if err := check(ps); err != nil {
		return nil, err
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Reads stay possible; a second sync waits.
	if _, err := tx.Exec(ctx, `LOCK TABLE plans IN EXCLUSIVE MODE`); err != nil {
		return nil, err
	}
	existing, err := listPlans(ctx, tx)
	if err != nil {
		return nil, err
	}
	byCode := map[string]Plan{}
	for _, p := range existing {
		byCode[p.Code] = p
	}

	var changes []string
	listed := map[string]bool{}
	for _, p := range ps {
		listed[p.Code] = true
		old, ok := byCode[p.Code]
		if !ok {
			if _, err := tx.Exec(ctx, `
				INSERT INTO plans (code, name, price_minor, currency, "interval",
					max_instances, max_storage_gb, max_connections, max_memory_mb, max_vcpu, public)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
				p.Code, p.Name, p.PriceMinor, p.Currency, p.Interval,
				p.Instances, p.StorageGB, p.Connections, p.MemoryMB, p.VCPU, p.Public); err != nil {
				return nil, fmt.Errorf("add plan %s: %w", p.Code, err)
			}
			changes = append(changes, "added plan "+p.Code)
			continue
		}
		if old.PriceMinor != p.PriceMinor || old.Currency != p.Currency || old.Interval != p.Interval {
			return nil, fmt.Errorf("plan %s: %w", p.Code, ErrPriceChanged)
		}
		var what []string
		if old.Name != p.Name {
			what = append(what, fmt.Sprintf("name %q → %q", old.Name, p.Name))
		}
		if old.Limits != p.Limits {
			what = append(what, fmt.Sprintf("limits %+v → %+v (new subscriptions only)", old.Limits, p.Limits))
		}
		if old.Public != p.Public {
			what = append(what, fmt.Sprintf("public %t → %t", old.Public, p.Public))
		}
		if !old.Active {
			what = append(what, "reactivated")
		}
		if len(what) == 0 {
			continue
		}
		if _, err := tx.Exec(ctx, `
			UPDATE plans SET name = $2, max_instances = $3, max_storage_gb = $4, max_connections = $5,
				max_memory_mb = $6, max_vcpu = $7, public = $8, active = true, updated_at = now()
			WHERE code = $1`,
			p.Code, p.Name, p.Instances, p.StorageGB, p.Connections, p.MemoryMB, p.VCPU, p.Public); err != nil {
			return nil, fmt.Errorf("update plan %s: %w", p.Code, err)
		}
		for _, w := range what {
			changes = append(changes, "plan "+p.Code+": "+w)
		}
	}
	for _, p := range existing {
		if p.Active && !listed[p.Code] {
			if _, err := tx.Exec(ctx, `UPDATE plans SET active = false, updated_at = now() WHERE code = $1`, p.Code); err != nil {
				return nil, fmt.Errorf("deactivate plan %s: %w", p.Code, err)
			}
			changes = append(changes, "deactivated plan "+p.Code)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return changes, nil
}

// ListPlans lists every plan, active or not, by code.
func ListPlans(ctx context.Context, db DB) ([]Plan, error) { return listPlans(ctx, db) }

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func listPlans(ctx context.Context, q querier) ([]Plan, error) {
	rows, err := q.Query(ctx, `
		SELECT code, name, price_minor, currency, "interval",
			max_instances, max_storage_gb, max_connections, max_memory_mb, max_vcpu, public, active
		FROM plans ORDER BY code`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Plan, error) {
		var p Plan
		err := row.Scan(&p.Code, &p.Name, &p.PriceMinor, &p.Currency, &p.Interval,
			&p.Instances, &p.StorageGB, &p.Connections, &p.MemoryMB, &p.VCPU, &p.Public, &p.Active)
		return p, err
	})
}
