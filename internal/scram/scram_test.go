package scram_test

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ottermq/otter_cloud/internal/scram"
)

func TestPassword(t *testing.T) {
	seen := map[string]bool{}
	counts := map[rune]int{}
	for range 2000 {
		p := scram.Password()
		if !regexp.MustCompile(`^[A-Za-z0-9]{32}$`).MatchString(p) {
			t.Fatalf("password %q", p)
		}
		if seen[p] {
			t.Fatalf("password %q twice", p)
		}
		seen[p] = true
		for _, r := range p {
			counts[r]++
		}
	}
	// 64000 characters over 62: about 1032 each. A biased draw, such as a
	// plain modulo of a byte, gives the first eight about 1290.
	for r, n := range counts {
		if n < 850 || n > 1220 {
			t.Errorf("%q drawn %d times of 64000", r, n)
		}
	}
	if len(counts) != 62 {
		t.Errorf("%d distinct characters, want 62", len(counts))
	}
}

// The verifier is checked the way a server checks a client's proof
// (RFC 5802 §3): the StoredKey must be H(HMAC(SaltedPassword, "Client
// Key")), and the ServerKey HMAC(SaltedPassword, "Server Key").
// PostgreSQL's acceptance is tested against a real cluster in
// internal/postgres.
func TestVerifier(t *testing.T) {
	v, err := scram.Verifier("pencil")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`^SCRAM-SHA-256\$([0-9]+):([A-Za-z0-9+/=]+)\$([A-Za-z0-9+/=]+):([A-Za-z0-9+/=]+)$`).FindStringSubmatch(v)
	if m == nil {
		t.Fatalf("verifier %q", v)
	}
	iter, _ := strconv.Atoi(m[1])
	dec := func(s string) []byte {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	salt, stored, server := dec(m[2]), dec(m[3]), dec(m[4])
	if iter != scram.Iterations || len(salt) != 16 {
		t.Errorf("iterations %d, salt %d bytes", iter, len(salt))
	}
	salted, _ := pbkdf2.Key(sha256.New, "pencil", salt, iter, 32)
	mac := func(msg string) []byte { h := hmac.New(sha256.New, salted); h.Write([]byte(msg)); return h.Sum(nil) }
	want := sha256.Sum256(mac("Client Key"))
	if !hmac.Equal(stored, want[:]) || !hmac.Equal(server, mac("Server Key")) {
		t.Error("the keys don't follow from the password")
	}
	if strings.Contains(v, "pencil") {
		t.Error("the verifier holds the password")
	}
	again, _ := scram.Verifier("pencil")
	if again == v {
		t.Error("two verifiers of one password are the same: the salt isn't fresh")
	}
}
