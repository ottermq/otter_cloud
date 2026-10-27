package identity

import (
	"errors"
	"net/mail"
	"strings"
)

// ErrInvalidEmail is safe to show.
var ErrInvalidEmail = errors.New("enter a valid email address, like name@example.com")

const maxEmailLength = 254 // RFC 5321's path limit

// NormalizeEmail trims and lowercases an address and checks it is a bare
// address (no display name). Lowercasing the local part is technically
// stricter than RFC 5321, but no mail provider a customer will use treats
// case as significant, and it keeps one person from holding two accounts.
func NormalizeEmail(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > maxEmailLength {
		return "", ErrInvalidEmail
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s || addr.Name != "" {
		return "", ErrInvalidEmail
	}
	at := strings.LastIndexByte(s, '@')
	if at < 1 || !strings.Contains(s[at+1:], ".") {
		return "", ErrInvalidEmail
	}
	return s, nil
}
