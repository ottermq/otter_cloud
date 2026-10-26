package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

const redacted = "[redacted]"

// Secret holds a value that must never reach a log, an error message or an
// encoded response (security.md R8). Every way of printing it prints
// "[redacted]"; Reveal is the one deliberate way to read it.
type Secret struct {
	value string
}

// NewSecret wraps a value.
func NewSecret(value string) Secret { return Secret{value: value} }

// Reveal returns the value. Call it only where the value is handed to the
// thing that needs it, such as a database driver.
func (s Secret) Reveal() string { return s.value }

// IsZero reports whether no value is set.
func (s Secret) IsZero() bool { return s.value == "" }

func (s Secret) String() string               { return redacted }
func (s Secret) GoString() string             { return redacted }
func (s Secret) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redacted)) }
func (s Secret) LogValue() slog.Value         { return slog.StringValue(redacted) }
func (s Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// readSecretFile reads a secret from a file that only its owner can read.
// Surrounding whitespace, such as the trailing newline an editor adds, is
// dropped. The file's contents never appear in an error.
func readSecretFile(name, path string) (Secret, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Secret{}, fmt.Errorf("%s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return Secret{}, fmt.Errorf("%s: %s is not a regular file", name, path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return Secret{}, fmt.Errorf("%s: %s is mode %04o; a secret file must not be readable by group or others (chmod 0400)", name, path, perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Secret{}, fmt.Errorf("%s: %w", name, err)
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return Secret{}, fmt.Errorf("%s: %s is empty", name, path)
	}
	return NewSecret(value), nil
}
