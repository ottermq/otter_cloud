package billing_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ottermq/otter_cloud/internal/billing"
	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/identity"
)

func founder() billing.Plan {
	return billing.Plan{Code: "founder-beta", Name: "Founder Beta", Currency: "BRL", Interval: "none",
		Limits: billing.Limits{Instances: 2, StorageGB: 10, Connections: 60}}
}

func paid() billing.Plan {
	return billing.Plan{Code: "pro", Name: "Pro", PriceMinor: 4900, Currency: "BRL", Interval: "month",
		Limits: billing.Limits{Instances: 5, StorageGB: 50, Connections: 200}, Public: true}
}

// TestSeedIsQ12 pins plans.json to the decision it comes from
// (open-questions.md Q12). Changing one means changing the other.
func TestSeedIsQ12(t *testing.T) {
	got, err := billing.Seed()
	if err != nil {
		t.Fatal(err)
	}
	if want := []billing.Plan{founder()}; !reflect.DeepEqual(got, want) {
		t.Errorf("seed = %+v, want %+v", got, want)
	}
}

// TestNoPlanCodesOutsideBilling is the rule that keeps "if founder" checks
// out of the code (domain-model.md §2.3): no non-test Go file outside this
// package names a plan code. Everything else asks AccountEntitlement.
func TestNoPlanCodesOutsideBilling(t *testing.T) {
	plans, err := billing.Seed()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join("..", "..")
	here, _ := filepath.Abs(".")
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if abs, _ := filepath.Abs(path); abs == here || strings.HasPrefix(d.Name(), ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, p := range plans {
			if strings.Contains(string(data), p.Code) {
				t.Errorf("%s names plan %q: ask billing.AccountEntitlement instead", path, p.Code)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

type fixture struct {
	t         *testing.T
	db        *dbtest.DB
	accountID string
}

// newFixture is a database with the founder and a paid plan, and one user.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := dbtest.New(t)
	if _, err := billing.SyncPlans(context.Background(), db.ConnAs(dbtest.RoleMigrator), []billing.Plan{founder(), paid()}); err != nil {
		t.Fatal(err)
	}
	u, err := identity.CreateUser(context.Background(), db.ConnAs(dbtest.RoleAPI), identity.NewUser{
		Email: "ana@example.com", Password: "correct horse battery staple"})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, db: db, accountID: u.AccountID}
}

func (f *fixture) entitlement() billing.Entitlement {
	f.t.Helper()
	e, err := billing.AccountEntitlement(context.Background(), f.db.ConnAs(dbtest.RoleAPI), f.accountID)
	if err != nil {
		f.t.Fatal(err)
	}
	return e
}

func checkViolation(t *testing.T, what string, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Errorf("%s: err = %v, want a check violation", what, err)
	}
}

func TestFreePlanIsEntitled(t *testing.T) {
	f := newFixture(t)
	if e := f.entitlement(); e.Entitled {
		t.Errorf("before subscribing: %+v, want not entitled", e)
	}
	s, err := billing.Subscribe(context.Background(), f.db.ConnAs(dbtest.RoleAdmin), f.accountID, "founder-beta")
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != "active" || s.PaymentStatus != "not_required" {
		t.Errorf("subscription = %+v", s)
	}
	e := f.entitlement()
	if !e.Entitled || e.Limits != founder().Limits || e.SubscriptionID != s.ID || e.PlanName != "Founder Beta" {
		t.Errorf("entitlement = %+v", e)
	}
}

// A paid plan can't become active without a verified payment event, and
// there is no billing provider to send one (Q11): not through billing, not
// through otter_admin's grants, not even as the table's owner.
func TestPaidPlanCannotActivate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.db.ConnAs(dbtest.RoleAdmin)
	s, err := billing.Subscribe(ctx, admin, f.accountID, "pro")
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != "pending" || s.PaymentStatus != "pending" {
		t.Errorf("subscription = %+v, want pending", s)
	}
	if e := f.entitlement(); e.Entitled {
		t.Errorf("entitlement = %+v, want not entitled while payment is pending", e)
	}

	_, err = admin.Exec(ctx, `UPDATE subscriptions SET status = 'active' WHERE id = $1`, s.ID)
	checkViolation(t, "admin activates", err)
	owner := f.db.ConnAs(dbtest.RoleMigrator)
	_, err = owner.Exec(ctx, `UPDATE subscriptions SET status = 'active', payment_status = 'current' WHERE id = $1`, s.ID)
	checkViolation(t, "owner marks paid", err)
	_, err = owner.Exec(ctx, `UPDATE subscriptions SET payment_status = 'not_required' WHERE id = $1`, s.ID)
	checkViolation(t, "owner waives payment", err)
	if _, err := admin.Exec(ctx, `UPDATE subscriptions SET payment_status = 'current' WHERE id = $1`, s.ID); err == nil ||
		!strings.Contains(err.Error(), "permission denied") {
		t.Errorf("admin sets payment_status: %v, want permission denied", err)
	}
	if e := f.entitlement(); e.Entitled {
		t.Errorf("entitlement = %+v, want still not entitled", e)
	}
}

func TestOneOpenSubscriptionPerAccount(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.db.ConnAs(dbtest.RoleAdmin)
	if _, err := billing.Cancel(ctx, admin, f.accountID); !errors.Is(err, billing.ErrNoOpenSubscription) {
		t.Errorf("cancel with none: %v", err)
	}
	if _, err := billing.Subscribe(ctx, admin, f.accountID, "founder-beta"); err != nil {
		t.Fatal(err)
	}
	if _, err := billing.Subscribe(ctx, admin, f.accountID, "pro"); !errors.Is(err, billing.ErrAlreadySubscribed) {
		t.Errorf("second subscription: %v, want ErrAlreadySubscribed", err)
	}
	c, err := billing.Cancel(ctx, admin, f.accountID)
	if err != nil || c.Status != "canceled" || c.EndsAt == nil {
		t.Fatalf("cancel = %+v, %v", c, err)
	}
	if e := f.entitlement(); e.Entitled {
		t.Errorf("after cancel: %+v, want not entitled", e)
	}
	if _, err := billing.Subscribe(ctx, admin, f.accountID, "founder-beta"); err != nil {
		t.Errorf("subscribe again after cancel: %v", err)
	}
	subs, err := billing.ListSubscriptions(ctx, admin)
	if err != nil || len(subs) != 2 || subs[0].Status != "active" || subs[1].Status != "canceled" {
		t.Errorf("list = %+v, %v; want the new one first, the canceled one kept", subs, err)
	}
	if _, err := billing.Subscribe(ctx, admin, f.accountID, "no-such-plan"); !errors.Is(err, billing.ErrPlanNotFound) {
		t.Errorf("unknown plan: %v", err)
	}
}

func TestEndedSubscriptionIsNotEntitled(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s, err := billing.Subscribe(ctx, f.db.ConnAs(dbtest.RoleAdmin), f.accountID, "founder-beta")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ConnAs(dbtest.RoleMigrator).Exec(ctx,
		`UPDATE subscriptions SET starts_at = now() - interval '2 days', ends_at = now() - interval '1 day' WHERE id = $1`, s.ID); err != nil {
		t.Fatal(err)
	}
	if e := f.entitlement(); e.Entitled {
		t.Errorf("entitlement = %+v, want not entitled after ends_at", e)
	}
}

func TestSyncPlans(t *testing.T) {
	db := dbtest.New(t) // already has the seed
	ctx := context.Background()
	mig := db.ConnAs(dbtest.RoleMigrator)

	if changes, err := billing.SyncPlans(ctx, mig, []billing.Plan{founder()}); err != nil || len(changes) != 0 {
		t.Errorf("same seed again = %q, %v; want no changes", changes, err)
	}
	changes, err := billing.SyncPlans(ctx, mig, []billing.Plan{founder(), paid()})
	if err != nil || !slices.Equal(changes, []string{"added plan pro"}) {
		t.Errorf("add = %q, %v", changes, err)
	}

	// A subscription keeps the limits it was created with.
	u, err := identity.CreateUser(ctx, db.ConnAs(dbtest.RoleAPI), identity.NewUser{
		Email: "ana@example.com", Password: "correct horse battery staple"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := billing.Subscribe(ctx, db.ConnAs(dbtest.RoleAdmin), u.AccountID, "founder-beta"); err != nil {
		t.Fatal(err)
	}
	bigger := founder()
	bigger.StorageGB = 20
	changes, err = billing.SyncPlans(ctx, mig, []billing.Plan{bigger})
	if err != nil || len(changes) != 2 || !strings.Contains(changes[0], "limits") || changes[1] != "deactivated plan pro" {
		t.Errorf("change limits, drop pro = %q, %v", changes, err)
	}
	e, err := billing.AccountEntitlement(ctx, db.ConnAs(dbtest.RoleAPI), u.AccountID)
	if err != nil || e.Limits.StorageGB != 10 {
		t.Errorf("entitlement = %+v, %v; want the 10 GB it was created with", e, err)
	}
	if _, err := billing.Subscribe(ctx, db.ConnAs(dbtest.RoleAdmin), u.AccountID, "pro"); !errors.Is(err, billing.ErrPlanNotFound) {
		t.Errorf("subscribe to a deactivated plan: %v", err)
	}

	pricier := bigger
	pricier.PriceMinor = 100
	if _, err := billing.SyncPlans(ctx, mig, []billing.Plan{pricier}); !errors.Is(err, billing.ErrPriceChanged) {
		t.Errorf("price change: %v, want ErrPriceChanged", err)
	}
	for name, p := range map[string]billing.Plan{
		"bad code":       {Code: "Founder", Name: "x", Currency: "BRL", Interval: "none"},
		"bad currency":   {Code: "x-plan", Name: "x", Currency: "real", Interval: "none"},
		"bad interval":   {Code: "x-plan", Name: "x", Currency: "BRL", Interval: "year"},
		"negative limit": {Code: "x-plan", Name: "x", Currency: "BRL", Interval: "none", Limits: billing.Limits{StorageGB: -1}},
		"no name":        {Code: "x-plan", Currency: "BRL", Interval: "none"},
	} {
		if _, err := billing.SyncPlans(ctx, mig, []billing.Plan{bigger, p}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBillingPrivileges(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	denied := func(role, sql string) {
		t.Helper()
		_, err := db.ConnAs(role).Exec(ctx, sql)
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s: %s = %v, want permission denied", role, sql, err)
		}
	}
	denied(dbtest.RoleAPI, `UPDATE plans SET max_storage_gb = 1000`)
	denied(dbtest.RoleAPI, `UPDATE subscriptions SET status = 'canceled'`)
	denied(dbtest.RoleAPI, `INSERT INTO subscriptions (account_id, plan_id, status, payment_status, requires_payment,
		max_instances, max_storage_gb, max_connections, max_memory_mb, max_vcpu)
		SELECT a.id, p.id, 'active', 'not_required', false, 1, 1, 1, 0, 0 FROM accounts a, plans p`)
	denied(dbtest.RoleAdmin, `UPDATE plans SET max_storage_gb = 1000`)
	denied(dbtest.RoleAdmin, `UPDATE subscriptions SET max_storage_gb = 1000`)
	denied(dbtest.RoleAdmin, `DELETE FROM subscriptions`)
	denied(dbtest.RoleWorker, `SELECT 1 FROM subscriptions`)
	denied(dbtest.RoleWorker, `SELECT 1 FROM plans`)
}
