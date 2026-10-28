package instances

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/billing"
	"github.com/ottermq/otter_cloud/internal/catalog"
	"github.com/ottermq/otter_cloud/internal/scram"
)

// Errors a customer can act on; their text is safe to show. The API maps
// them to statuses: not entitled → 403, quota and name → 409, no capacity
// → 503, the rest → 422.
var (
	ErrAccountNotActive    = errors.New("the account is not active")
	ErrNotEntitled         = errors.New("the account has no active subscription")
	ErrInvalidName         = errors.New("a name is 2–63 lowercase letters, digits and hyphens, starting with a letter")
	ErrInstanceNameTaken   = errors.New("you already have a database with this name")
	ErrNoCapacity          = errors.New("there is no room for a new database right now; try again later")
	ErrIdempotencyMismatch = errors.New("this Idempotency-Key was already used for a different request")
)

// QuotaError means the instance would take the account past one of its
// subscription's limits. It names the limit.
type QuotaError struct {
	Limit     string // "instances", "storage_gb", "connections", "memory_mb", "vcpu"
	Allowed   int
	Used      int
	Requested int
}

func (e *QuotaError) Error() string {
	return fmt.Sprintf("over quota: %s allows %d, you have %d, and this needs %d more", e.Limit, e.Allowed, e.Used, e.Requested)
}

// CreateRequest is a request for a new instance.
type CreateRequest struct {
	AccountID      string
	Name           string
	FlavorCode     string
	RequestedBy    string // an audit actor: user:<id>, operator:<login>
	IdempotencyKey string // optional
}

// Instance is a resource an account owns (domain-model.md Instance).
type Instance struct {
	ID             string
	AccountID      string
	Name           string
	ServiceType    catalog.ServiceType
	FlavorCode     string
	StorageGB      int
	MaxConnections *int
	ClusterID      string
	NodeID         string
	InfraName      string
	DesiredState   string
	Status         string
	Health         string
	CreatedAt      time.Time
}

// Operation is a queued job for an instance (domain-model.md Operation).
type Operation struct {
	ID         string
	InstanceID string
	Kind       string
	Status     string
	CreatedAt  time.Time
}

// Created is what Create made, or found. Password is the customer's,
// generated for this create and kept nowhere: the caller shows it in its
// response and nowhere else. Replayed means the request's Idempotency-Key
// named an earlier create, and nothing new was made; Password is then
// empty, since only the first response had it.
type Created struct {
	Instance  Instance
	Operation Operation
	Password  string
	Replayed  bool
}

var instanceNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)

// Create records a new shared database and queues its create operation,
// after checking, in this order and under the locks domain-model.md §5
// describes: the account is active (its row locked, so the account's
// creates serialise), its subscription entitles it, the flavor exists, the
// account's quota has room, and a cluster has capacity (its lock held while
// it is chosen). Nothing touches a node: a worker runs the operation later.
//
// tx must be a transaction; the caller adds the audit event and commits.
// On an error the caller rolls back.
func Create(ctx context.Context, tx pgx.Tx, r CreateRequest) (Created, error) {
	if !instanceNamePattern.MatchString(r.Name) {
		return Created{}, ErrInvalidName
	}
	var accountStatus string
	err := tx.QueryRow(ctx, `SELECT status FROM accounts WHERE id = $1 FOR UPDATE`, r.AccountID).Scan(&accountStatus)
	if err != nil {
		return Created{}, fmt.Errorf("lock account: %w", err)
	}
	if accountStatus != "active" {
		return Created{}, ErrAccountNotActive
	}

	// A replay is answered before anything else is checked: the request
	// already succeeded, even if the quota is now full.
	if r.IdempotencyKey != "" {
		c, found, err := replay(ctx, tx, r)
		if found || err != nil {
			return c, err
		}
	}

	ent, err := billing.AccountEntitlement(ctx, tx, r.AccountID)
	if err != nil {
		return Created{}, err
	}
	if !ent.Entitled {
		return Created{}, ErrNotEntitled
	}
	f, err := catalog.ActiveFlavor(ctx, tx, r.FlavorCode)
	if err != nil {
		return Created{}, err
	}
	if f.ServiceType != catalog.PostgreSQL || f.MaxConnections == nil {
		return Created{}, fmt.Errorf("flavor %s is not a shared database", f.Code)
	}
	if err := checkQuota(ctx, tx, r.AccountID, ent.Limits, f); err != nil {
		return Created{}, err
	}
	var taken bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT FROM instances WHERE account_id = $1 AND name = $2 AND status <> 'deleted')`,
		r.AccountID, r.Name).Scan(&taken); err != nil {
		return Created{}, fmt.Errorf("check name: %w", err)
	}
	if taken {
		return Created{}, ErrInstanceNameTaken
	}

	alloc := Allocation{StorageGB: f.StorageGB, Connections: *f.MaxConnections}
	cluster, ok, err := placeShared(ctx, tx, alloc)
	if err != nil {
		return Created{}, err
	}
	if !ok {
		return Created{}, ErrNoCapacity
	}

	infraName, err := newInfraName()
	if err != nil {
		return Created{}, err
	}
	in := Instance{AccountID: r.AccountID, Name: r.Name, ServiceType: f.ServiceType, FlavorCode: f.Code,
		StorageGB: f.StorageGB, MaxConnections: f.MaxConnections, ClusterID: cluster.ID, NodeID: cluster.NodeID,
		InfraName: infraName}
	err = tx.QueryRow(ctx, `
		INSERT INTO instances (account_id, name, service_type, flavor_code, storage_gb, max_connections,
			cluster_id, node_id, infra_name)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, desired_state, status, health, created_at`,
		in.AccountID, in.Name, in.ServiceType, in.FlavorCode, in.StorageGB, in.MaxConnections,
		in.ClusterID, in.NodeID, in.InfraName).
		Scan(&in.ID, &in.DesiredState, &in.Status, &in.Health, &in.CreatedAt)
	if err != nil {
		return Created{}, fmt.Errorf("insert instance: %w", err)
	}
	// Only the verifier is stored, in the payload the worker reads; the
	// queue clears it when the operation finishes.
	password := scram.Password()
	verifier, err := scram.Verifier(password)
	if err != nil {
		return Created{}, err
	}
	op := Operation{InstanceID: in.ID, Kind: "create"}
	err = tx.QueryRow(ctx, `
		INSERT INTO operations (instance_id, account_id, node_id, kind, requested_by, idempotency_key, payload)
		VALUES ($1, $2, $3, 'create', $4, NULLIF($5, ''), jsonb_build_object('verifier', $6::text))
		RETURNING id, status, created_at`,
		in.ID, in.AccountID, in.NodeID, r.RequestedBy, r.IdempotencyKey, verifier).
		Scan(&op.ID, &op.Status, &op.CreatedAt)
	if err != nil {
		return Created{}, fmt.Errorf("insert operation: %w", err)
	}
	return Created{Instance: in, Operation: op, Password: password}, nil
}

// replay finds the create an Idempotency-Key already named. A key reused
// for a different name or flavor is refused rather than answered with the
// wrong instance.
func replay(ctx context.Context, tx pgx.Tx, r CreateRequest) (Created, bool, error) {
	var c Created
	in, op := &c.Instance, &c.Operation
	err := tx.QueryRow(ctx, `
		SELECT o.kind, `+instanceColumns+`, o.id, o.instance_id, o.status, o.created_at
		FROM operations o JOIN instances i ON i.id = o.instance_id
		WHERE o.account_id = $1 AND o.idempotency_key = $2`, r.AccountID, r.IdempotencyKey).
		Scan(append([]any{&op.Kind}, append(instanceDest(in), &op.ID, &op.InstanceID, &op.Status, &op.CreatedAt)...)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Created{}, false, nil
	}
	if err != nil {
		return Created{}, false, fmt.Errorf("look up Idempotency-Key: %w", err)
	}
	if op.Kind != "create" || in.Name != r.Name || in.FlavorCode != r.FlavorCode {
		return Created{}, true, ErrIdempotencyMismatch
	}
	c.Replayed = true
	return c, true, nil
}

// instanceColumns are an instance's columns, for a query that calls the
// table i.
const instanceColumns = `i.id, i.account_id, i.name, i.service_type, i.flavor_code, i.storage_gb, i.max_connections,
	i.cluster_id, i.node_id, i.infra_name, i.desired_state, i.status, i.health, i.created_at`

func instanceDest(in *Instance) []any {
	return []any{&in.ID, &in.AccountID, &in.Name, &in.ServiceType, &in.FlavorCode, &in.StorageGB, &in.MaxConnections,
		&in.ClusterID, &in.NodeID, &in.InfraName, &in.DesiredState, &in.Status, &in.Health, &in.CreatedAt}
}

// checkQuota compares what the account holds, plus f, with its limits.
func checkQuota(ctx context.Context, tx pgx.Tx, accountID string, l billing.Limits, f catalog.Flavor) error {
	u, err := AccountUsage(ctx, tx, accountID)
	if err != nil {
		return err
	}
	for _, c := range []struct {
		limit                    string
		allowed, used, requested int
	}{
		{"instances", l.Instances, u.Instances, 1},
		{"storage_gb", l.StorageGB, u.StorageGB, f.StorageGB},
		{"connections", l.Connections, u.Connections, deref(f.MaxConnections)},
		{"memory_mb", l.MemoryMB, u.MemoryMB, deref(f.MemoryMB)},
		{"vcpu", l.VCPU, u.VCPU, deref(f.VCPU)},
	} {
		if c.requested > 0 && c.used+c.requested > c.allowed {
			return &QuotaError{Limit: c.limit, Allowed: c.allowed, Used: c.used, Requested: c.requested}
		}
	}
	return nil
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// newInfraName is a fresh database and role name, otc_ and 48 random bits.
func newInfraName() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate a name: %w", err)
	}
	return "otc_" + hex.EncodeToString(b), nil
}
