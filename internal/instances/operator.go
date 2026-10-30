package instances

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// OperatorInstance is an instance as an operator lists it (`otter admin
// instances list`, #102): whose it is and where it lives, which a customer
// never sees.
type OperatorInstance struct {
	ID           string
	Owner        string // the account's first user's email
	Name         string
	Flavor       string
	Status       string
	DesiredState string
	Reason       string
	Node         string
	Cluster      string // "" for an instance outside a cluster
	InfraName    string
	CreatedAt    time.Time
}

// OperatorInstances returns instances, newest first; deleted ones only with
// withDeleted.
func OperatorInstances(ctx context.Context, db DB, withDeleted bool) ([]OperatorInstance, error) {
	rows, err := db.Query(ctx, `
		SELECT i.id, coalesce((SELECT min(u.email) FROM users u WHERE u.account_id = i.account_id), ''), i.name,
			i.flavor_code, i.status, i.desired_state, coalesce(i.reason, ''), n.name, coalesce(c.name, ''),
			i.infra_name, i.created_at
		FROM instances i JOIN infrastructure_nodes n ON n.id = i.node_id LEFT JOIN clusters c ON c.id = i.cluster_id
		WHERE $1 OR i.status <> 'deleted'
		ORDER BY i.created_at DESC, i.id DESC`, withDeleted)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (OperatorInstance, error) {
		var v OperatorInstance
		err := row.Scan(&v.ID, &v.Owner, &v.Name, &v.Flavor, &v.Status, &v.DesiredState, &v.Reason, &v.Node,
			&v.Cluster, &v.InfraName, &v.CreatedAt)
		return v, err
	})
}
