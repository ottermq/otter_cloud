package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"testing"

	"filippo.io/age"
)

// What is sealed only the identity opens, and the plaintext's size and hash
// are the input's.
func TestSeal(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	rs, err := ParseRecipients("# the operator's offline key\n" + id.Recipient().String() + "\n")
	if err != nil {
		t.Fatal(err)
	}
	plain := bytes.Repeat([]byte("PGDMP customer rows "), 10000)
	dir := t.TempDir()
	sealed, err := (Sealer{Recipients: rs}).Seal(bytes.NewReader(plain), dir)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(plain)
	if sealed.PlainSize != int64(len(plain)) || sealed.PlainSHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("plain %d %s, want the input's", sealed.PlainSize, sealed.PlainSHA256)
	}
	if _, err := sealed.Body.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	ciphertext, _ := io.ReadAll(sealed.Body)
	if int64(len(ciphertext)) != sealed.Size || bytes.Contains(ciphertext, []byte("customer rows")) {
		t.Errorf("sealed %d bytes (recorded %d), or the plaintext shows", len(ciphertext), sealed.Size)
	}
	r, err := age.Decrypt(bytes.NewReader(ciphertext), id)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(r); !bytes.Equal(got, plain) {
		t.Error("unsealed is not the input")
	}
	other, _ := age.GenerateX25519Identity()
	if _, err := age.Decrypt(bytes.NewReader(ciphertext), other); err == nil {
		t.Error("another identity opened it")
	}
	if err := sealed.Close(); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("%d files left after Close", len(entries))
	}
}

func TestParseRecipients(t *testing.T) {
	for _, text := range []string{"", "# nothing\n", "not a key\n"} {
		if _, err := ParseRecipients(text); err == nil {
			t.Errorf("%q: accepted", text)
		}
	}
}
