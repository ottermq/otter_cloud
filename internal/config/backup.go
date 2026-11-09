package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// The backup target's settings (#118, architecture.md §10): the worker's,
// for its node's dumps, and otter backup's, for whatever it is run on. Each
// has its own, so moving to another target is a change of settings.
const (
	EnvBackupTarget            = "OTTER_BACKUP_TARGET"
	EnvBackupTargetName        = "OTTER_BACKUP_TARGET_NAME"
	EnvBackupRecipientsFile    = "OTTER_BACKUP_RECIPIENTS_FILE"
	EnvBackupFSDir             = "OTTER_BACKUP_FS_DIR"
	EnvBackupS3URL             = "OTTER_BACKUP_S3_URL"
	EnvBackupS3Region          = "OTTER_BACKUP_S3_REGION"
	EnvBackupS3CredentialsFile = "OTTER_BACKUP_S3_CREDENTIALS_FILE"
	EnvBackupMaxBytes          = "OTTER_BACKUP_MAX_BYTES"
)

const defaultS3Region = "us-east-1"

// Backup is where backups go off the node, and sealed to whom.
type Backup struct {
	// Target is none (the default), fs or s3.
	Target string
	// Name is the target's in backup_records: the kind unless set, so two
	// targets of a kind used one after the other can be told apart.
	Name string
	// Recipients are the age recipients copies are sealed to, one a line,
	// from OTTER_BACKUP_RECIPIENTS_FILE. Public keys: their private keys
	// are never on the node.
	Recipients     string
	RecipientsFile string
	// FSDir is the fs target's directory.
	FSDir string
	// S3Endpoint, S3Bucket and S3Prefix come from OTTER_BACKUP_S3_URL,
	// https://host[:port]/bucket[/prefix].
	S3Endpoint            *url.URL
	S3Bucket, S3Prefix    string
	S3Region, S3AccessKey string
	S3SecretKey           Secret
	// MaxBytes is the most the copies on the target may add up to; 0, no
	// limit.
	MaxBytes int64
}

var (
	targetName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	s3Part     = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
	byteSize   = regexp.MustCompile(`^([0-9]+)([KMG]?)$`)
)

func loadBackup(getenv func(string) string, fail func(error)) Backup {
	b := Backup{Target: getenv(EnvBackupTarget)}
	if b.Target == "" {
		b.Target = "none"
	}
	switch b.Target {
	case "none", "fs", "s3":
	default:
		fail(fmt.Errorf("%s must be none, fs or s3", EnvBackupTarget))
		return b
	}
	b.Name = getenv(EnvBackupTargetName)
	if b.Name == "" {
		b.Name = b.Target
	}
	if !targetName.MatchString(b.Name) || b.Name == "local" {
		fail(fmt.Errorf("%s must be a lowercase name of up to 32 letters, digits, '_' and '-', and not local", EnvBackupTargetName))
	}
	if b.Target == "none" {
		return b
	}

	if path := getenv(EnvBackupRecipientsFile); path == "" {
		fail(fmt.Errorf("%s is required: nothing leaves the node unsealed", EnvBackupRecipientsFile))
	} else if data, err := os.ReadFile(path); err != nil {
		fail(fmt.Errorf("%s: %w", EnvBackupRecipientsFile, err))
	} else if strings.Contains(string(data), "AGE-SECRET-KEY-") {
		fail(fmt.Errorf("%s: %s holds a private key; it must hold only recipients (public keys), the private ones kept off the node", EnvBackupRecipientsFile, path))
	} else {
		b.Recipients, b.RecipientsFile = string(data), path
	}

	if v := getenv(EnvBackupMaxBytes); v != "" {
		n, err := parseByteSize(v)
		if err != nil {
			fail(fmt.Errorf("%s: %w", EnvBackupMaxBytes, err))
		}
		b.MaxBytes = n
	}

	switch b.Target {
	case "fs":
		b.FSDir = getenv(EnvBackupFSDir)
		if b.FSDir == "" {
			fail(fmt.Errorf("%s is required with %s=fs", EnvBackupFSDir, EnvBackupTarget))
		} else if err := checkPrivateDir(EnvBackupFSDir, b.FSDir); err != nil {
			fail(err)
		}
	case "s3":
		if err := parseS3URL(getenv(EnvBackupS3URL), &b); err != nil {
			fail(err)
		}
		b.S3Region = getenv(EnvBackupS3Region)
		if b.S3Region == "" {
			b.S3Region = defaultS3Region
		}
		if path := getenv(EnvBackupS3CredentialsFile); path == "" {
			fail(fmt.Errorf("%s is required with %s=s3", EnvBackupS3CredentialsFile, EnvBackupTarget))
		} else if s, err := readSecretFile(EnvBackupS3CredentialsFile, path); err != nil {
			fail(err)
		} else if id, secret, ok := strings.Cut(s.Reveal(), ":"); !ok || id == "" || secret == "" {
			fail(fmt.Errorf("%s: %s must hold <access key id>:<secret access key>", EnvBackupS3CredentialsFile, path))
		} else {
			b.S3AccessKey, b.S3SecretKey = id, NewSecret(secret)
		}
	}
	return b
}

// parseS3URL reads https://host[:port]/bucket[/prefix]: http only on
// localhost, no credentials, query or fragment.
func parseS3URL(v string, b *Backup) error {
	if v == "" {
		return fmt.Errorf("%s is required with %s=s3", EnvBackupS3URL, EnvBackupTarget)
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s must be https://host[:port]/bucket[/prefix], without credentials", EnvBackupS3URL)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
		return fmt.Errorf("%s must be https:// (http:// only on localhost)", EnvBackupS3URL)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for _, p := range parts {
		if !s3Part.MatchString(p) {
			return fmt.Errorf("%s: the bucket and prefix must be names of letters, digits, '.', '_' and '-'", EnvBackupS3URL)
		}
	}
	b.S3Bucket = parts[0]
	if len(parts) > 1 {
		b.S3Prefix = strings.Join(parts[1:], "/") + "/"
	}
	b.S3Endpoint = &url.URL{Scheme: u.Scheme, Host: u.Host}
	return nil
}

// parseByteSize reads a number of bytes, with an optional K, M or G
// (binary).
func parseByteSize(v string) (int64, error) {
	m := byteSize.FindStringSubmatch(v)
	if m == nil {
		return 0, fmt.Errorf("%q is not a size such as 1073741824 or 1G", v)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a size such as 1073741824 or 1G", v)
	}
	shift := map[string]uint{"": 0, "K": 10, "M": 20, "G": 30}[m[2]]
	if n > (1<<62)>>shift {
		return 0, fmt.Errorf("%q is too large", v)
	}
	return n << shift, nil
}

// LogValue describes the target without its secret.
func (b Backup) LogValue() slog.Value {
	attrs := []slog.Attr{slog.String("target", b.Target), slog.String("name", b.Name)}
	switch b.Target {
	case "fs":
		attrs = append(attrs, slog.String("dir", b.FSDir))
	case "s3":
		attrs = append(attrs, slog.String("endpoint", b.S3Endpoint.String()), slog.String("bucket", b.S3Bucket),
			slog.String("prefix", b.S3Prefix), slog.String("region", b.S3Region))
	}
	if b.MaxBytes > 0 {
		attrs = append(attrs, slog.Int64("max_bytes", b.MaxBytes))
	}
	return slog.GroupValue(attrs...)
}
