package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrEmailTaken is safe to show to an operator; a sign-up form should not
// reveal it to the public.
var ErrEmailTaken = errors.New("a user with this email address already exists")

// DB is what identity needs from the platform database: a *pgx.Conn,
// *pgxpool.Pool or pgx.Tx.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// User is a person who logs in. In the MVP each user owns one account.
type User struct {
	ID         string
	AccountID  string
	Email      string
	Status     string
	IsOperator bool
	CreatedAt  time.Time
}

// NewUser is what CreateUser needs.
type NewUser struct {
	Email       string
	Password    string
	AccountName string // defaults to the email address
	IsOperator  bool
}

// CreateUser creates an account and its owner in one transaction, after
// normalising the email and applying the password policy. Policy and email
// errors are returned as they are, so callers can show them.
func CreateUser(ctx context.Context, db DB, n NewUser) (User, error) {
	email, err := NormalizeEmail(n.Email)
	if err != nil {
		return User{}, err
	}
	if err := CheckPassword(n.Password, email); err != nil {
		return User{}, err
	}
	name := strings.TrimSpace(n.AccountName)
	if name == "" {
		name = email
	}
	hash, err := HashPassword(ctx, n.Password)
	if err != nil {
		return User{}, err
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	u := User{Email: email, IsOperator: n.IsOperator}
	if err := tx.QueryRow(ctx,
		"INSERT INTO accounts (name) VALUES ($1) RETURNING id",
		name).Scan(&u.AccountID); err != nil {
		return User{}, fmt.Errorf("create account: %w", err)
	}
	err = tx.QueryRow(ctx,
		`INSERT INTO users (account_id, email, password_hash, is_operator)
		 VALUES ($1, $2, $3, $4) RETURNING id, status, created_at`,
		u.AccountID, email, hash, n.IsOperator).Scan(&u.ID, &u.Status, &u.CreatedAt)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" && pgErr.ConstraintName == "users_email_key" {
		return User{}, ErrEmailTaken
	}
	if err != nil {
		return User{}, fmt.Errorf("create user: %w", err)
	}
	return u, tx.Commit(ctx)
}
