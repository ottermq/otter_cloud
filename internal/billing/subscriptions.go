package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Errors an operator can act on.
var (
	ErrPlanNotFound       = errors.New("no active plan with this code")
	ErrAlreadySubscribed  = errors.New("the account already has a subscription; cancel it first")
	ErrNoOpenSubscription = errors.New("the account has no subscription to cancel")
)

// Subscription links an account to a plan, with the plan's limits as they
// were when it was created (domain-model.md Subscription).
type Subscription struct {
	ID            string
	AccountID     string
	PlanCode      string
	PlanName      string
	Status        string // pending, active, suspended, canceled, expired
	PaymentStatus string // not_required, pending, current, past_due
	Limits        Limits
	StartsAt      time.Time
	EndsAt        *time.Time
	CreatedAt     time.Time
}

// Subscribe gives the account a subscription to the active plan with code.
// A free plan starts active. A paid plan starts pending, and stays so until
// a verified payment event makes its payment current: the table refuses an
// active subscription that owes money, and with no billing provider yet
// nothing can make a payment current (Q11). Run it in a transaction with
// the audit event.
func Subscribe(ctx context.Context, db DB, accountID, planCode string) (Subscription, error) {
	var (
		planID string
		plan   Plan
	)
	err := db.QueryRow(ctx, `
		SELECT id, code, name, price_minor, max_instances, max_storage_gb, max_connections, max_memory_mb, max_vcpu
		FROM plans WHERE code = $1 AND active`, planCode).
		Scan(&planID, &plan.Code, &plan.Name, &plan.PriceMinor,
			&plan.Instances, &plan.StorageGB, &plan.Connections, &plan.MemoryMB, &plan.VCPU)
	if errors.Is(err, pgx.ErrNoRows) {
		return Subscription{}, ErrPlanNotFound
	}
	if err != nil {
		return Subscription{}, fmt.Errorf("look up plan: %w", err)
	}

	s := Subscription{AccountID: accountID, PlanCode: plan.Code, PlanName: plan.Name, Limits: plan.Limits,
		Status: "active", PaymentStatus: "not_required"}
	if plan.RequiresPayment() {
		s.Status, s.PaymentStatus = "pending", "pending"
	}
	l := plan.Limits
	err = db.QueryRow(ctx, `
		INSERT INTO subscriptions (account_id, plan_id, status, payment_status, requires_payment,
			max_instances, max_storage_gb, max_connections, max_memory_mb, max_vcpu)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id, starts_at, created_at`,
		accountID, planID, s.Status, s.PaymentStatus, plan.RequiresPayment(),
		l.Instances, l.StorageGB, l.Connections, l.MemoryMB, l.VCPU).
		Scan(&s.ID, &s.StartsAt, &s.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.ConstraintName == "subscriptions_one_open_per_account" {
		return Subscription{}, ErrAlreadySubscribed
	}
	if err != nil {
		return Subscription{}, fmt.Errorf("create subscription: %w", err)
	}
	return s, nil
}

// Cancel ends the account's open subscription now. Its instances are not
// touched: what happens to them is the expiry policy's job (Q7). Run it in
// a transaction with the audit event.
func Cancel(ctx context.Context, db DB, accountID string) (Subscription, error) {
	rows, err := db.Query(ctx, `
		UPDATE subscriptions s SET status = 'canceled', ends_at = now(), updated_at = now()
		FROM plans p
		WHERE p.id = s.plan_id AND s.account_id = $1 AND s.status IN ('pending', 'active', 'suspended')
		RETURNING `+subscriptionColumns, accountID)
	if err != nil {
		return Subscription{}, fmt.Errorf("cancel subscription: %w", err)
	}
	s, err := pgx.CollectExactlyOneRow(rows, scanSubscription)
	if errors.Is(err, pgx.ErrNoRows) {
		return Subscription{}, ErrNoOpenSubscription
	}
	if err != nil {
		return Subscription{}, fmt.Errorf("cancel subscription: %w", err)
	}
	return s, nil
}

// ListSubscriptions lists every subscription, newest first.
func ListSubscriptions(ctx context.Context, db DB) ([]Subscription, error) {
	rows, err := db.Query(ctx, `
		SELECT `+subscriptionColumns+`
		FROM subscriptions s JOIN plans p ON p.id = s.plan_id
		ORDER BY s.created_at DESC, s.id DESC`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanSubscription)
}

const subscriptionColumns = `s.id, s.account_id, p.code, p.name, s.status, s.payment_status,
	s.max_instances, s.max_storage_gb, s.max_connections, s.max_memory_mb, s.max_vcpu,
	s.starts_at, s.ends_at, s.created_at`

func scanSubscription(row pgx.CollectableRow) (Subscription, error) {
	var s Subscription
	l := &s.Limits
	err := row.Scan(&s.ID, &s.AccountID, &s.PlanCode, &s.PlanName, &s.Status, &s.PaymentStatus,
		&l.Instances, &l.StorageGB, &l.Connections, &l.MemoryMB, &l.VCPU,
		&s.StartsAt, &s.EndsAt, &s.CreatedAt)
	return s, err
}

// Entitlement is what an account may hold right now. It is the one answer
// every other module asks for; none of them looks at plans, prices or
// payment states (domain-model.md §2.3).
type Entitlement struct {
	Entitled       bool
	Limits         Limits // zero when not entitled
	SubscriptionID string
	PlanName       string // for display only; never branch on it
}

// AccountEntitlement computes the account's entitlement from its single
// open subscription: entitled when it is active, its payment requirement is
// met, and it hasn't ended. Called inside the transaction that checks quota
// (AD-8), it sees the same snapshot.
func AccountEntitlement(ctx context.Context, db DB, accountID string) (Entitlement, error) {
	var e Entitlement
	l := &e.Limits
	err := db.QueryRow(ctx, `
		SELECT s.id, p.name, s.max_instances, s.max_storage_gb, s.max_connections, s.max_memory_mb, s.max_vcpu
		FROM subscriptions s JOIN plans p ON p.id = s.plan_id
		WHERE s.account_id = $1
		  AND s.status = 'active'
		  AND s.payment_status IN ('not_required', 'current')
		  AND s.starts_at <= now()
		  AND (s.ends_at IS NULL OR s.ends_at > now())`, accountID).
		Scan(&e.SubscriptionID, &e.PlanName, &l.Instances, &l.StorageGB, &l.Connections, &l.MemoryMB, &l.VCPU)
	if errors.Is(err, pgx.ErrNoRows) {
		return Entitlement{}, nil
	}
	if err != nil {
		return Entitlement{}, fmt.Errorf("compute entitlement: %w", err)
	}
	e.Entitled = true
	return e, nil
}
