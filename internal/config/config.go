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
	"net/netip"
	"net/url"
	"os"
	"strings"
)

// The settings. Each is documented in .env.sample and the README.
const (
	EnvDatabaseURL          = "OTTER_DATABASE_URL"
	EnvDatabasePasswordFile = "OTTER_DATABASE_PASSWORD_FILE"
	EnvListenAddr           = "OTTER_LISTEN_ADDR"
	EnvPublicURL            = "OTTER_PUBLIC_URL"
	EnvLogLevel             = "OTTER_LOG_LEVEL"
	EnvTrustedProxies       = "OTTER_TRUSTED_PROXIES"
	EnvMetricsAddr          = "OTTER_METRICS_ADDR"
	EnvJailDeckSocket       = "OTTER_JAILDECK_SOCKET"
	EnvJailDeckTokenFile    = "OTTER_JAILDECK_TOKEN_FILE"
	EnvClusterCertFile      = "OTTER_CLUSTER_ADMIN_CERT_FILE"
	EnvClusterKeyFile       = "OTTER_CLUSTER_ADMIN_KEY_FILE"
)

const (
	defaultListenAddr  = "127.0.0.1:8080"
	defaultMetricsAddr = "127.0.0.1:9101"
	// The worker's, apart from the api's: both may run on one host.
	defaultWorkerMetrics  = "127.0.0.1:9102"
	defaultJailDeckSocket = "/var/run/jaildeck.sock"
)

// Needs says which settings a subcommand uses. Settings it doesn't use are
// neither read nor validated, so a worker never fails over an api setting.
type Needs struct {
	Database  bool // OTTER_DATABASE_URL, OTTER_DATABASE_PASSWORD_FILE
	HTTP      bool // OTTER_LISTEN_ADDR, OTTER_TRUSTED_PROXIES, OTTER_METRICS_ADDR
	PublicURL bool // OTTER_PUBLIC_URL
	JailDeck  bool // OTTER_JAILDECK_SOCKET, OTTER_JAILDECK_TOKEN_FILE: the worker only
	Cluster   bool // OTTER_CLUSTER_ADMIN_CERT_FILE, OTTER_CLUSTER_ADMIN_KEY_FILE: the worker only
	Metrics   bool // OTTER_METRICS_ADDR alone, for the worker; HTTP includes it
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
	// MetricsAddr is the internal listener for /metrics; empty when
	// OTTER_METRICS_ADDR is "off".
	MetricsAddr string
	// TrustedProxies are the addresses (the edge's) whose X-Forwarded-For
	// is believed. Empty means clients connect directly.
	TrustedProxies []netip.Prefix
	// PublicURL is the address customers use: invite links, the CSRF origin
	// check and the cookie's Secure flag derive from it.
	PublicURL *url.URL

	// JailDeckSocket is the JailDeck unix socket on this node.
	JailDeckSocket string
	// JailDeckToken is root-equivalent on the node (JD-003), from
	// OTTER_JAILDECK_TOKEN_FILE.
	JailDeckToken Secret

	// ClusterCert and ClusterKey are the node's client certificate for the
	// clusters' admin role, PEM (AD-6). Only the key is secret.
	ClusterCert []byte
	ClusterKey  Secret

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
		maddr, err := parseMetricsAddr(getenv(EnvMetricsAddr), defaultMetricsAddr)
		if err != nil {
			fail(err)
		}
		cfg.MetricsAddr = maddr
		if maddr != "" && maddr == addr {
			fail(fmt.Errorf("%s and %s must differ: /metrics must not be on the public listener", EnvMetricsAddr, EnvListenAddr))
		}
		proxies, err := parseTrustedProxies(getenv(EnvTrustedProxies))
		if err != nil {
			fail(err)
		}
		cfg.TrustedProxies = proxies
	}

	if needs.PublicURL {
		u, err := parsePublicURL(getenv(EnvPublicURL))
		if err != nil {
			fail(err)
		}
		cfg.PublicURL = u
	}

	if needs.Metrics && !needs.HTTP {
		maddr, err := parseMetricsAddr(getenv(EnvMetricsAddr), defaultWorkerMetrics)
		if err != nil {
			fail(err)
		}
		cfg.MetricsAddr = maddr
	}

	if needs.JailDeck {
		cfg.JailDeckSocket = getenv(EnvJailDeckSocket)
		if cfg.JailDeckSocket == "" {
			cfg.JailDeckSocket = defaultJailDeckSocket
		}
		if !strings.HasPrefix(cfg.JailDeckSocket, "/") {
			fail(fmt.Errorf("%s must be an absolute path", EnvJailDeckSocket))
		}
		if path := getenv(EnvJailDeckTokenFile); path == "" {
			fail(fmt.Errorf("%s is required", EnvJailDeckTokenFile))
		} else if s, err := readSecretFile(EnvJailDeckTokenFile, path); err != nil {
			fail(err)
		} else {
			cfg.JailDeckToken = s
		}
	}

	if needs.Cluster {
		if path := getenv(EnvClusterCertFile); path == "" {
			fail(fmt.Errorf("%s is required", EnvClusterCertFile))
		} else if b, err := os.ReadFile(path); err != nil {
			fail(fmt.Errorf("%s: %w", EnvClusterCertFile, err))
		} else {
			cfg.ClusterCert = b
		}
		if path := getenv(EnvClusterKeyFile); path == "" {
			fail(fmt.Errorf("%s is required", EnvClusterKeyFile))
		} else if s, err := readSecretFile(EnvClusterKeyFile, path); err != nil {
			fail(err)
		} else {
			cfg.ClusterKey = s
		}
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
	if c.MetricsAddr != "" {
		attrs = append(attrs, slog.String("metrics_addr", c.MetricsAddr))
	}
	if len(c.TrustedProxies) > 0 {
		attrs = append(attrs, slog.Any("trusted_proxies", c.TrustedProxies))
	}
	if c.PublicURL != nil {
		attrs = append(attrs, slog.String("public_url", c.PublicURL.String()))
	}
	if c.JailDeckSocket != "" {
		attrs = append(attrs, slog.String("jaildeck_socket", c.JailDeckSocket),
			slog.Bool("jaildeck_token_set", !c.JailDeckToken.IsZero()))
	}
	if len(c.ClusterCert) > 0 {
		attrs = append(attrs, slog.Bool("cluster_admin_key_set", !c.ClusterKey.IsZero()))
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

func parseMetricsAddr(v, def string) (string, error) {
	switch v {
	case "":
		return def, nil
	case "off":
		return "", nil
	}
	if _, _, err := net.SplitHostPort(v); err != nil {
		return "", fmt.Errorf("%s: %q is not host:port or off", EnvMetricsAddr, v)
	}
	return v, nil
}

// parseTrustedProxies reads a comma-separated list of addresses and CIDR
// prefixes, such as the edge's WireGuard address.
func parseTrustedProxies(v string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(v, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !strings.Contains(f, "/") {
			a, err := netip.ParseAddr(f)
			if err != nil {
				return nil, fmt.Errorf("%s: %q is not an address or CIDR prefix", EnvTrustedProxies, f)
			}
			out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not an address or CIDR prefix", EnvTrustedProxies, f)
		}
		if p.Bits() == 0 {
			return nil, fmt.Errorf("%s: %q trusts every address; list the proxy's own addresses", EnvTrustedProxies, f)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}
