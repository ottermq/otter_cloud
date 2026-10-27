// Package config reads otter's settings from the environment and validates
// them at startup.
//
// Plain settings are OTTER_* environment variables. Secrets are never taken
// from the environment: a *_FILE variable names a file only its owner can
// read (security.md R8). Each subcommand asks for the settings it uses, and
// Load reports every problem at once, each naming its variable.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
)

// The settings. Each is documented in .env.sample and the README.
const (
	EnvDatabaseURL          = "OTTER_DATABASE_URL"
	EnvDatabasePasswordFile = "OTTER_DATABASE_PASSWORD_FILE"
	EnvListenAddr           = "OTTER_LISTEN_ADDR"
	EnvPublicURL            = "OTTER_PUBLIC_URL"
	EnvLogLevel             = "OTTER_LOG_LEVEL"
)

const defaultListenAddr = "127.0.0.1:8080"

// Needs says which settings a subcommand uses. Settings it doesn't use are
// neither read nor validated, so a worker never fails over an api setting.
type Needs struct {
	Database  bool // OTTER_DATABASE_URL, OTTER_DATABASE_PASSWORD_FILE
	HTTP      bool // OTTER_LISTEN_ADDR
	PublicURL bool // OTTER_PUBLIC_URL
}

// Config is the validated configuration. Fields a subcommand did not ask
// for are left zero.
type Config struct {
	// DatabaseURL is a postgres:// URL without a password.
	DatabaseURL *url.URL
	// DatabasePassword comes from OTTER_DATABASE_PASSWORD_FILE. It may be
	// empty when the server authenticates otherwise (a certificate, peer).
	DatabasePassword Secret

	// ListenAddr is the api's host:port.
	ListenAddr string
	// PublicURL is the address customers use: invite links, the CSRF origin
	// check and the cookie's Secure flag derive from it.
	PublicURL *url.URL

	LogLevel slog.Level
}

// Load reads and validates the settings needs asks for, using getenv
// (os.Getenv in production). The error, if any, lists every problem.
func Load(getenv func(string) string, needs Needs) (*Config, error) {
	var cfg Config
	var errs []error
	fail := func(err error) { errs = append(errs, err) }

	level, err := parseLogLevel(getenv(EnvLogLevel))
	if err != nil {
		fail(err)
	}
	cfg.LogLevel = level

	if needs.Database {
		u, err := parseDatabaseURL(getenv(EnvDatabaseURL))
		if err != nil {
			fail(err)
		}
		cfg.DatabaseURL = u
		if path := getenv(EnvDatabasePasswordFile); path != "" {
			s, err := readSecretFile(EnvDatabasePasswordFile, path)
			if err != nil {
				fail(err)
			}
			cfg.DatabasePassword = s
		}
	}

	if needs.HTTP {
		addr, err := parseListenAddr(getenv(EnvListenAddr))
		if err != nil {
			fail(err)
		}
		cfg.ListenAddr = addr
	}

	if needs.PublicURL {
		u, err := parsePublicURL(getenv(EnvPublicURL))
		if err != nil {
			fail(err)
		}
		cfg.PublicURL = u
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return &cfg, nil
}

// LogValue lets the configuration be logged at startup without leaking the
// password.
func (c *Config) LogValue() slog.Value {
	attrs := []slog.Attr{slog.String("log_level", c.LogLevel.String())}
	if c.DatabaseURL != nil {
		attrs = append(attrs,
			slog.String("database_url", c.DatabaseURL.String()),
			slog.Bool("database_password_set", !c.DatabasePassword.IsZero()))
	}
	if c.ListenAddr != "" {
		attrs = append(attrs, slog.String("listen_addr", c.ListenAddr))
	}
	if c.PublicURL != nil {
		attrs = append(attrs, slog.String("public_url", c.PublicURL.String()))
	}
	return slog.GroupValue(attrs...)
}

func parseLogLevel(v string) (slog.Level, error) {
	switch strings.ToLower(v) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, fmt.Errorf("%s: %q is not one of debug, info, warn, error", EnvLogLevel, v)
}

func parseDatabaseURL(v string) (*url.URL, error) {
	if v == "" {
		return nil, fmt.Errorf("%s is required (e.g. postgres://otter_api@127.0.0.1:5432/otter?sslmode=verify-full)", EnvDatabaseURL)
	}
	u, err := url.Parse(v)
	if err != nil {
		// url.Parse quotes the input in its error, which may hold a password.
		return nil, fmt.Errorf("%s is not a valid URL", EnvDatabaseURL)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return nil, fmt.Errorf("%s: scheme must be postgres://, got %q", EnvDatabaseURL, u.Scheme)
	}
	if _, set := u.User.Password(); set {
		return nil, fmt.Errorf("%s must not contain a password; put it in a file named by %s", EnvDatabaseURL, EnvDatabasePasswordFile)
	}
	if u.Query().Has("password") {
		return nil, fmt.Errorf("%s must not contain a password parameter; use %s", EnvDatabaseURL, EnvDatabasePasswordFile)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%s: no host", EnvDatabaseURL)
	}
	if u.User.Username() == "" {
		return nil, fmt.Errorf("%s: no user; each process connects as its own role", EnvDatabaseURL)
	}
	if strings.Trim(u.Path, "/") == "" {
		return nil, fmt.Errorf("%s: no database name", EnvDatabaseURL)
	}
	return u, nil
}

func parseListenAddr(v string) (string, error) {
	if v == "" {
		return defaultListenAddr, nil
	}
	if _, _, err := net.SplitHostPort(v); err != nil {
		return "", fmt.Errorf("%s: %q is not host:port", EnvListenAddr, v)
	}
	return v, nil
}

func parsePublicURL(v string) (*url.URL, error) {
	if v == "" {
		return nil, fmt.Errorf("%s is required (e.g. https://cloud.otter-labs.com)", EnvPublicURL)
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("%s: %q is not an absolute URL", EnvPublicURL, v)
	}
	switch u.Scheme {
	case "https":
	case "http":
		// Plain HTTP only for local development: the session cookie is
		// Secure, so a browser would drop it anywhere else.
		if !isLoopbackHost(u.Hostname()) {
			return nil, fmt.Errorf("%s: http:// is allowed only for localhost; use https://", EnvPublicURL)
		}
	default:
		return nil, fmt.Errorf("%s: scheme must be https://, got %q", EnvPublicURL, u.Scheme)
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%s: must be an origin without a path, e.g. https://cloud.otter-labs.com", EnvPublicURL)
	}
	u.Path = ""
	return u, nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
