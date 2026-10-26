// Package platformdb connects to the platform database.
package platformdb

import (
	"context"
	"fmt"
	"net/url"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/config"
)

// ConnConfig builds a connection config from the configured URL and the
// password read from its file. The password is set on the config, never put
// into a URL, so it can't surface in a logged connection string.
func ConnConfig(u *url.URL, password config.Secret) (*pgx.ConnConfig, error) {
	cc, err := pgx.ParseConfig(u.String())
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", config.EnvDatabaseURL, err)
	}
	if !password.IsZero() {
		cc.Password = password.Reveal()
	}
	return cc, nil
}

// Connect opens one connection to the platform database.
func Connect(ctx context.Context, cfg *config.Config) (*pgx.Conn, error) {
	cc, err := ConnConfig(cfg.DatabaseURL, cfg.DatabasePassword)
	if err != nil {
		return nil, err
	}
	conn, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		return nil, fmt.Errorf("connect to the platform database as %s: %w", cc.User, err)
	}
	return conn, nil
}
