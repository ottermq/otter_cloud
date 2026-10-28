package identity

import (
	"encoding/base64"
	"fmt"
	"testing"

	"golang.org/x/crypto/argon2"
)

func encodeForTest(t *testing.T, password string, salt []byte, memory, time uint32, threads uint8) string {
	t.Helper()
	key := argon2.IDKey([]byte(password), salt, time, memory, threads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, memory, time, threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// HashForTest makes a hash with other parameters, for the external tests.
func HashForTest(t *testing.T, password string, memory, time uint32, threads uint8) string {
	return encodeForTest(t, password, make([]byte, 16), memory, time, threads)
}
