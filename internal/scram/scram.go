// Package scram makes customer database passwords and their SCRAM-SHA-256
// verifiers (RFC 5802, RFC 7677), in the form PostgreSQL stores. otter api
// generates the password, shows it once, and keeps only the verifier,
// which is all the worker and the cluster ever see (architecture.md §6).
package scram

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// PasswordLength is a generated password's length: 32 characters of 62,
// about 190 bits.
const PasswordLength = 32

// Iterations is PostgreSQL's default scram_iterations. A generated password
// is too long to guess, so more would only slow every login.
const Iterations = 4096

const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// Password is a new random password: letters and digits only, so it needs
// no quoting in a connection URL or a shell, and SASLprep leaves it as is.
func Password() string {
	b := make([]byte, PasswordLength)
	buf := make([]byte, 1)
	for i := range b {
		for {
			_, _ = rand.Read(buf) // crypto/rand never fails
			// 248 = 4 × 62: rejecting above it keeps every letter equally likely.
			if buf[0] < 248 {
				b[i] = alphabet[buf[0]%62]
				break
			}
		}
	}
	return string(b)
}

// Verifier is password's SCRAM-SHA-256 verifier, with a fresh salt:
// SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>. The password
// is used as given, without SASLprep, which is exact for Password's.
func Verifier(password string) (string, error) {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	salted, err := pbkdf2.Key(sha256.New, password, salt, Iterations, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("scram: %w", err)
	}
	stored := sha256.Sum256(mac(salted, "Client Key"))
	b := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", Iterations, b(salt), b(stored[:]), b(mac(salted, "Server Key"))), nil
}

func mac(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}
