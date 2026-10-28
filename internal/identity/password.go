package identity

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters: RFC 9106's second recommended option, the same as
// JailDeck. Recorded in security.md; raising them later only affects new
// hashes, and NeedsRehash finds the old ones at login.
const (
	argonMemory  uint32 = 64 * 1024 // KiB, so 64 MiB
	argonTime    uint32 = 3
	argonThreads uint8  = 4
	argonSaltLen        = 16
	argonKeyLen  uint32 = 32
)

// Password policy (security.md, NIST SP 800-63B): a minimum length and a
// list of common passwords, and no composition rules.
const (
	MinPasswordLength = 12   // characters
	maxPasswordBytes  = 1024 // bounds the work one request can ask for
)

// Errors a person can act on. Their text is safe to show.
var (
	ErrPasswordTooShort = fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	ErrPasswordTooLong  = fmt.Errorf("password must be at most %d bytes", maxPasswordBytes)
	ErrPasswordCommon   = errors.New("password is too common: it appears in lists of passwords attackers try first")
	ErrPasswordIsEmail  = errors.New("password must not be your email address")
)

// hashSlots bounds concurrent hashing: each hash holds 64 MiB, so a burst of
// sign-ups or logins could otherwise exhaust the control plane's memory.
var hashSlots = make(chan struct{}, 4)

// common-passwords.txt is regenerated from SecLists (MIT) with:
//
//	cat 100k-most-used-passwords-NCSC.txt Portugese_Pwdb_common-password-list-top-150.txt |
//	  tr -d '\r' | LC_ALL=C grep -E '^[ -~]{12,}$' | grep -vF '$hex[' |
//	  tr 'A-Z' 'a-z' | LC_ALL=C sort -u
//
// Only entries of at least MinPasswordLength characters matter; shorter ones
// are already refused. Non-ASCII entries are left out: tr cannot lowercase
// them, and they are rare in the sources.
//
//go:embed common-passwords.txt
var commonPasswordsFile string

var commonPasswords = func() map[string]struct{} {
	m := map[string]struct{}{}
	s := bufio.NewScanner(strings.NewReader(commonPasswordsFile))
	for s.Scan() {
		if line := s.Text(); line != "" && !strings.HasPrefix(line, "#") {
			m[line] = struct{}{}
		}
	}
	return m
}()

// CheckPassword applies the password policy. email is the account's
// address, which may not be the password.
func CheckPassword(password, email string) error {
	if len(password) > maxPasswordBytes {
		return ErrPasswordTooLong
	}
	if utf8.RuneCountInString(password) < MinPasswordLength {
		return ErrPasswordTooShort
	}
	lower := strings.ToLower(password)
	if _, ok := commonPasswords[lower]; ok || oneRepeatedCharacter(password) {
		return ErrPasswordCommon
	}
	if email != "" && lower == strings.ToLower(email) {
		return ErrPasswordIsEmail
	}
	return nil
}

func oneRepeatedCharacter(s string) bool {
	first, _ := utf8.DecodeRuneInString(s)
	return strings.Trim(s, string(first)) == ""
}

// HashPassword returns the argon2id hash of password in the PHC string
// format ($argon2id$v=19$m=…,t=…,p=…$salt$hash). It does not apply the
// policy; call CheckPassword first.
func HashPassword(ctx context.Context, password string) (string, error) {
	if len(password) > maxPasswordBytes {
		return "", ErrPasswordTooLong
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	if err := acquire(ctx); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	release()
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches encoded. The comparison
// is constant-time. A malformed hash is an error, not a mismatch.
func VerifyPassword(ctx context.Context, password, encoded string) (bool, error) {
	if len(password) > maxPasswordBytes {
		return false, nil
	}
	p, err := parseHash(encoded)
	if err != nil {
		return false, err
	}
	if err := acquire(ctx); err != nil {
		return false, err
	}
	key := argon2.IDKey([]byte(password), p.salt, p.time, p.memory, p.threads, uint32(len(p.key)))
	release()
	return subtle.ConstantTimeCompare(key, p.key) == 1, nil
}

// NeedsRehash reports whether encoded was made with other parameters than
// the current ones, so a successful login can store a fresh hash.
func NeedsRehash(encoded string) bool {
	p, err := parseHash(encoded)
	if err != nil {
		return true
	}
	return p.memory != argonMemory || p.time != argonTime || p.threads != argonThreads ||
		len(p.salt) != argonSaltLen || uint32(len(p.key)) != argonKeyLen
}

func acquire(ctx context.Context) error {
	select {
	case hashSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func release() { <-hashSlots }

var errMalformedHash = errors.New("malformed password hash")

type hashParams struct {
	memory, time uint32
	threads      uint8
	salt, key    []byte
}

// parseHash reads a PHC string. The bounds are sanity checks against a
// corrupted or hostile row, not policy: a stored hash must not be able to
// ask for gigabytes of memory.
func parseHash(encoded string) (hashParams, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return hashParams{}, errMalformedHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return hashParams{}, errMalformedHash
	}
	var p hashParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return hashParams{}, errMalformedHash
	}
	if p.memory < 8*1024 || p.memory > 1024*1024 || p.time < 1 || p.time > 10 || p.threads < 1 || p.threads > 16 {
		return hashParams{}, errMalformedHash
	}
	var err error
	if p.salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil || len(p.salt) < 8 || len(p.salt) > 64 {
		return hashParams{}, errMalformedHash
	}
	if p.key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil || len(p.key) < 16 || len(p.key) > 64 {
		return hashParams{}, errMalformedHash
	}
	return p, nil
}
