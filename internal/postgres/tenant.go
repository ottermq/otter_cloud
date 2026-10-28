package postgres

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// Tenant is one customer database in a shared cluster: a role and a
// database of the same platform-generated name, which reveal nothing about
// the customer (architecture.md §6).
type Tenant struct {
	Name string // otc_<12 hex>, the instance's infra_name
	// Verifier is the customer password's SCRAM verifier. The password
	// itself never reaches the worker (security.md §6).
	Verifier  string
	StorageGB int // the flavor's; temp_file_limit
}

// Per-database settings from Q12. statement_timeout and
// idle_in_transaction_session_timeout are defaults a tenant may change in
// its session; temp_file_limit is superuser-only, so it holds.
const (
	StatementTimeout = "60s"
	IdleInTxTimeout  = "5min"
	// ServerPool is how many server connections PgBouncer opens for one
	// database (Q12), and so the role's CONNECTION LIMIT: tenants reach
	// PostgreSQL only through PgBouncer.
	ServerPool = 5
)

var (
	namePattern = regexp.MustCompile(`^otc_[0-9a-f]{12}$`)
	// SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>, base64.
	// Nothing in it can close a quoted literal.
	verifierPattern = regexp.MustCompile(`^SCRAM-SHA-256\$[0-9]{4,7}:[A-Za-z0-9+/]+={0,2}\$[A-Za-z0-9+/]+={0,2}:[A-Za-z0-9+/]+={0,2}$`)
)

// ErrUnsafeRole means a role or database of the tenant's name exists with
// something a tenant must never have. The worker stops rather than repair
// it: a platform name in that state is not something it made.
var ErrUnsafeRole = errors.New("the tenant's role or database exists with privileges a tenant must not have")

// check re-validates what the worker is about to use: it came from the
// platform DB, which a compromised api could have written (security.md R4).
func (t Tenant) check() error {
	switch {
	case !namePattern.MatchString(t.Name):
		return fmt.Errorf("tenant name %q is not otc_<12 hex>", t.Name)
	case !verifierPattern.MatchString(t.Verifier):
		return errors.New("the password verifier is not a SCRAM-SHA-256 verifier")
	case t.StorageGB < 1 || t.StorageGB > 10000:
		return fmt.Errorf("storage %d GB is out of range", t.StorageGB)
	}
	return nil
}

// EnsureRole makes the tenant's login role, or checks the one there:
//
//   - LOGIN, and none of SUPERUSER, CREATEDB, CREATEROLE, REPLICATION,
//     BYPASSRLS, nor membership in any role (Q10);
//   - CONNECTION LIMIT ServerPool and the verifier as its password;
//   - the settings of Q12, as role defaults;
//   - granted to AdminRole, with INHERIT and SET, so the worker can reach
//     and dump its database once CONNECT is revoked from PUBLIC.
//
// conn is an admin connection to the maintenance database.
func EnsureRole(ctx context.Context, conn *pgx.Conn, t Tenant) error {
	if err := t.check(); err != nil {
		return err
	}
	role := pgx.Identifier{t.Name}.Sanitize()
	exists, err := roleExists(ctx, conn, t.Name)
	if err != nil {
		return err
	}
	if !exists {
		if _, err := conn.Exec(ctx, "CREATE ROLE "+role+
			" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT"+
			" CONNECTION LIMIT "+strconv.Itoa(ServerPool)+" PASSWORD '"+t.Verifier+"'"); err != nil {
			return fmt.Errorf("create role %s: %w", t.Name, err)
		}
	} else if err := checkRole(ctx, conn, t.Name); err != nil {
		return err
	}
	for _, sql := range []string{
		// Repeating these is harmless, and repairs a run that stopped halfway.
		"ALTER ROLE " + role + " CONNECTION LIMIT " + strconv.Itoa(ServerPool) + " PASSWORD '" + t.Verifier + "'",
		"GRANT " + role + " TO " + AdminRole + " WITH INHERIT TRUE, SET TRUE",
		"ALTER ROLE " + role + " SET statement_timeout = '" + StatementTimeout + "'",
		"ALTER ROLE " + role + " SET idle_in_transaction_session_timeout = '" + IdleInTxTimeout + "'",
		"ALTER ROLE " + role + " SET temp_file_limit = '" + strconv.Itoa(t.StorageGB) + "GB'",
	} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			return fmt.Errorf("role %s: %w", t.Name, err)
		}
	}
	return checkRole(ctx, conn, t.Name)
}

// EnsureDatabase makes the tenant's database, owned by its role, or checks
// the one there; then revokes CONNECT from PUBLIC and, inside it, all on
// schema public from PUBLIC (the default since PostgreSQL 15, kept
// explicit). EnsureRole comes first. connect opens an admin connection to
// a database of the cluster by name.
func EnsureDatabase(ctx context.Context, conn *pgx.Conn, connect func(ctx context.Context, database string) (*pgx.Conn, error), t Tenant) error {
	if err := t.check(); err != nil {
		return err
	}
	name := pgx.Identifier{t.Name}.Sanitize()
	var owner string
	err := conn.QueryRow(ctx, `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = $1`, t.Name).Scan(&owner)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// CREATE DATABASE can't run in a transaction; a crash between it
		// and the steps below leaves a database that the next run finishes.
		if _, err := conn.Exec(ctx, "CREATE DATABASE "+name+" OWNER "+name+" TEMPLATE template0 ENCODING 'UTF8'"); err != nil {
			return fmt.Errorf("create database %s: %w", t.Name, err)
		}
	case err != nil:
		return fmt.Errorf("look up database %s: %w", t.Name, err)
	case owner != t.Name:
		return fmt.Errorf("database %s is owned by %s: %w", t.Name, owner, ErrUnsafeRole)
	}
	if _, err := conn.Exec(ctx, "REVOKE CONNECT, TEMPORARY ON DATABASE "+name+" FROM PUBLIC"); err != nil {
		return fmt.Errorf("database %s: %w", t.Name, err)
	}
	db, err := connect(ctx, t.Name)
	if err != nil {
		return err
	}
	defer db.Close(ctx)
	// As the owner, which is who may change the schema's privileges.
	for _, sql := range []string{
		"SET ROLE " + name,
		"REVOKE ALL ON SCHEMA public FROM PUBLIC",
		"GRANT TEMPORARY ON DATABASE " + name + " TO " + name,
		"RESET ROLE",
	} {
		if _, err := db.Exec(ctx, sql); err != nil {
			return fmt.Errorf("database %s: %w", t.Name, err)
		}
	}
	return nil
}

func roleExists(ctx context.Context, conn *pgx.Conn, name string) (bool, error) {
	var exists bool
	err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT FROM pg_roles WHERE rolname = $1)`, name).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("look up role %s: %w", name, err)
	}
	return exists, nil
}

// checkRole refuses a tenant role with any attribute or membership a
// tenant must not have.
func checkRole(ctx context.Context, conn *pgx.Conn, name string) error {
	var bad []string
	err := conn.QueryRow(ctx, `
		SELECT array_remove(ARRAY[
			CASE WHEN rolsuper THEN 'superuser' END,
			CASE WHEN rolcreatedb THEN 'createdb' END,
			CASE WHEN rolcreaterole THEN 'createrole' END,
			CASE WHEN rolreplication THEN 'replication' END,
			CASE WHEN rolbypassrls THEN 'bypassrls' END,
			CASE WHEN NOT rolcanlogin THEN 'nologin' END]
			|| ARRAY(SELECT 'member of ' || g.rolname FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid WHERE m.member = r.oid),
			NULL)
		FROM pg_roles r WHERE rolname = $1`, name).Scan(&bad)
	if err != nil {
		return fmt.Errorf("check role %s: %w", name, err)
	}
	if len(bad) > 0 {
		return fmt.Errorf("role %s: %v: %w", name, bad, ErrUnsafeRole)
	}
	return nil
}
