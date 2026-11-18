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
	"regexp"
	"strings"
	"time"

	"github.com/ottermq/otter_cloud/internal/legal"
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
	EnvEdgeAddr             = "OTTER_EDGE_ADDR"
	EnvDBDomain             = "OTTER_DB_DOMAIN"
	EnvLegalController      = "OTTER_LEGAL_CONTROLLER"
	EnvLegalContact         = "OTTER_LEGAL_CONTACT"
	EnvLegalEdgeCountry     = "OTTER_LEGAL_EDGE_COUNTRY"
	EnvLegalPublished       = "OTTER_LEGAL_PUBLISHED"
	EnvJailDeckSocket       = "OTTER_JAILDECK_SOCKET"
	EnvJailDeckTokenFile    = "OTTER_JAILDECK_TOKEN_FILE"
	EnvClusterCertFile      = "OTTER_CLUSTER_ADMIN_CERT_FILE"
	EnvClusterKeyFile       = "OTTER_CLUSTER_ADMIN_KEY_FILE"
	EnvDumpDir              = "OTTER_DUMP_DIR"
	EnvSnapshotEvery        = "OTTER_SNAPSHOT_EVERY"
	EnvSnapshotKeep         = "OTTER_SNAPSHOT_KEEP"
)

const (
	defaultListenAddr  = "127.0.0.1:8080"
	defaultMetricsAddr = "127.0.0.1:9101"
	// The worker's, apart from the api's: both may run on one host.
	defaultWorkerMetrics  = "127.0.0.1:9102"
	defaultJailDeckSocket = "/var/run/jaildeck.sock"
	defaultDumpDir        = "/var/db/otter/dumps"
	defaultSnapshotKeep   = 7 * 24 * time.Hour
	// minSnapshotEvery keeps a typo (1s for 1h) from snapshotting the
	// clusters every minute.
	minSnapshotEvery = 15 * time.Minute
)

// Needs says which settings a subcommand uses. Settings it doesn't use are
// neither read nor validated, so a worker never fails over an api setting.
type Needs struct {
	Database  bool // OTTER_DATABASE_URL, OTTER_DATABASE_PASSWORD_FILE
	HTTP      bool // OTTER_LISTEN_ADDR, OTTER_TRUSTED_PROXIES, OTTER_METRICS_ADDR, OTTER_EDGE_ADDR, OTTER_DB_DOMAIN, OTTER_LEGAL_*
	PublicURL bool // OTTER_PUBLIC_URL
	JailDeck  bool // OTTER_JAILDECK_SOCKET, OTTER_JAILDECK_TOKEN_FILE: the worker only
	Cluster   bool // OTTER_CLUSTER_ADMIN_CERT_FILE, OTTER_CLUSTER_ADMIN_KEY_FILE: the worker only
	Metrics   bool // OTTER_METRICS_ADDR alone, for the worker; HTTP includes it
	Dumps     bool // OTTER_DUMP_DIR: the worker only
	Backup    bool // OTTER_BACKUP_*: the worker and otter backup
	Snapshots bool // OTTER_SNAPSHOT_*: the worker only
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
	// EdgeAddr is the listener for the edge's routes (#103); empty when
	// OTTER_EDGE_ADDR is unset or "off".
	EdgeAddr string
	// DBDomain is the zone databases' public hostnames go under, e.g.
	// db.example.com; empty, new databases get no endpoint.
	DBDomain string
	// PublicURL is the address customers use: invite links, the CSRF origin
	// check and the cookie's Secure flag derive from it.
	PublicURL *url.URL
	// Legal are the fields the legal texts leave out (#125); all empty, the
	// texts aren't published. internal/legal validates them.
	Legal legal.Fields

	// JailDeckSocket is the JailDeck unix socket on this node.
	JailDeckSocket string
	// JailDeckToken is root-equivalent on the node (JD-003), from
	// OTTER_JAILDECK_TOKEN_FILE.
	JailDeckToken Secret

	// ClusterCert and ClusterKey are the node's client certificate for the
	// clusters' admin role, PEM (AD-6). Only the key is secret.
	ClusterCert []byte
	ClusterKey  Secret
	// ClusterCertFile and ClusterKeyFile are where they were read from, for
	// pg_dump, which reads them itself.
	ClusterCertFile, ClusterKeyFile string

	// DumpDir is where the worker keeps final dumps until the backup target
	// takes over (Q26): on the node, outside any jail, readable by the
	// worker's user alone.
	DumpDir string

	// Backup is the backup target (#118).
	Backup Backup

	// Snapshots is the worker's schedule of cluster snapshots (#116).
	Snapshots Snapshots

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
		eaddr, err := parseEdgeAddr(getenv(EnvEdgeAddr))
		if err != nil {
			fail(err)
		}
		cfg.EdgeAddr = eaddr
		if eaddr != "" && (eaddr == addr || eaddr == maddr) {
			fail(fmt.Errorf("%s must differ from %s and %s: the edge's routes have a listener of their own", EnvEdgeAddr, EnvListenAddr, EnvMetricsAddr))
		}
		domain, err := parseDBDomain(getenv(EnvDBDomain))
		if err != nil {
			fail(err)
		}
		cfg.DBDomain = domain
		if eaddr != "" && len(proxies) == 0 {
			fail(fmt.Errorf("%s needs %s: the edge's routes are served to the edge's address alone", EnvEdgeAddr, EnvTrustedProxies))
		}
		if eaddr != "" && domain == "" {
			fail(fmt.Errorf("%s needs %s: a route is a database's hostname", EnvEdgeAddr, EnvDBDomain))
		}
		cfg.Legal = legal.Fields{
			Controller:  strings.TrimSpace(getenv(EnvLegalController)),
			Contact:     strings.TrimSpace(getenv(EnvLegalContact)),
			EdgeCountry: strings.TrimSpace(getenv(EnvLegalEdgeCountry)),
			Published:   strings.TrimSpace(getenv(EnvLegalPublished)),
		}
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

	if needs.Dumps {
		cfg.DumpDir = getenv(EnvDumpDir)
		if cfg.DumpDir == "" {
			cfg.DumpDir = defaultDumpDir
		}
		if err := checkPrivateDir(EnvDumpDir, cfg.DumpDir); err != nil {
			fail(err)
		}
	}

	if needs.Backup {
		cfg.Backup = loadBackup(getenv, fail)
	}

	if needs.Snapshots {
		cfg.Snapshots = loadSnapshots(getenv, fail)
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
			cfg.ClusterCert, cfg.ClusterCertFile = b, path
		}
		if path := getenv(EnvClusterKeyFile); path == "" {
			fail(fmt.Errorf("%s is required", EnvClusterKeyFile))
		} else if s, err := readSecretFile(EnvClusterKeyFile, path); err != nil {
			fail(err)
		} else {
			cfg.ClusterKey, cfg.ClusterKeyFile = s, path
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
	if c.EdgeAddr != "" {
		attrs = append(attrs, slog.String("edge_addr", c.EdgeAddr))
	}
	if c.DBDomain != "" {
		attrs = append(attrs, slog.String("db_domain", c.DBDomain))
	}
	if c.PublicURL != nil {
		attrs = append(attrs, slog.String("public_url", c.PublicURL.String()))
	}
	if !c.Legal.IsZero() {
		// The version only: the name and contact stay out of the logs.
		attrs = append(attrs, slog.String("legal_published", c.Legal.Published))
	}
	if c.JailDeckSocket != "" {
		attrs = append(attrs, slog.String("jaildeck_socket", c.JailDeckSocket),
			slog.Bool("jaildeck_token_set", !c.JailDeckToken.IsZero()))
	}
	if len(c.ClusterCert) > 0 {
		attrs = append(attrs, slog.Bool("cluster_admin_key_set", !c.ClusterKey.IsZero()))
	}
	if c.DumpDir != "" {
		attrs = append(attrs, slog.String("dump_dir", c.DumpDir))
	}
	if c.Backup.Target != "" {
		attrs = append(attrs, slog.Any("backup", c.Backup))
	}
	if c.Snapshots.Every > 0 {
		attrs = append(attrs, slog.String("snapshot_every", c.Snapshots.Every.String()),
			slog.String("snapshot_keep", c.Snapshots.Keep.String()))
	}
	return slog.GroupValue(attrs...)
}

// Snapshots is how often the worker snapshots each cluster on its node
// through JailDeck, and how long it keeps each snapshot (#116). Every is
// zero, off, unless set: a node whose replication tool already snapshots
// the jails' datasets leaves it off (architecture.md §10).
type Snapshots struct {
	Every time.Duration
	Keep  time.Duration
}

func loadSnapshots(getenv func(string) string, fail func(error)) Snapshots {
	var s Snapshots
	switch every := getenv(EnvSnapshotEvery); every {
	case "", "off":
		if getenv(EnvSnapshotKeep) != "" {
			fail(fmt.Errorf("%s is set but %s is off", EnvSnapshotKeep, EnvSnapshotEvery))
		}
		return s
	default:
		d, err := time.ParseDuration(every)
		if err != nil || d < minSnapshotEvery {
			fail(fmt.Errorf("%s must be off or a duration of at least %s, such as 1h", EnvSnapshotEvery, minSnapshotEvery))
			return s
		}
		s.Every = d
	}
	s.Keep = defaultSnapshotKeep
	if keep := getenv(EnvSnapshotKeep); keep != "" {
		d, err := time.ParseDuration(keep)
		if err != nil || d < s.Every {
			fail(fmt.Errorf("%s must be a duration of at least %s (%s), such as 168h", EnvSnapshotKeep, EnvSnapshotEvery, s.Every))
			return Snapshots{}
		}
		s.Keep = d
	}
	return s
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

// parseEdgeAddr reads OTTER_EDGE_ADDR: host:port, or empty or "off" for
// no edge listener.
func parseEdgeAddr(v string) (string, error) {
	if v == "" || v == "off" {
		return "", nil
	}
	if _, _, err := net.SplitHostPort(v); err != nil {
		return "", fmt.Errorf("%s: %q is not host:port or off", EnvEdgeAddr, v)
	}
	return v, nil
}

// dnsLabel is one label of a hostname, lower case.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// parseDBDomain reads OTTER_DB_DOMAIN: a DNS name of two labels or more,
// lower case, without a trailing dot. Every database's hostname is one
// more label under it, which the edge's wildcard certificate covers.
func parseDBDomain(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	labels := strings.Split(v, ".")
	if len(labels) < 2 || len(v) > 200 {
		return "", fmt.Errorf("%s: %q is not a domain such as db.example.com", EnvDBDomain, v)
	}
	for _, l := range labels {
		if !dnsLabel.MatchString(l) {
			return "", fmt.Errorf("%s: %q is not a lower-case domain such as db.example.com", EnvDBDomain, v)
		}
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
