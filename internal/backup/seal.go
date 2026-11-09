package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"
)

// Sealer encrypts what leaves the node, with age, to recipients whose
// private keys are kept off it (architecture.md §10): no target sees
// plaintext, and the node can't read back what it sent.
type Sealer struct {
	Recipients []age.Recipient
}

// ParseRecipients reads age recipients, one a line; blank lines and lines
// starting with # are skipped. There must be at least one.
func ParseRecipients(text string) ([]age.Recipient, error) {
	rs, err := age.ParseRecipients(strings.NewReader(text))
	if err != nil {
		return nil, err
	}
	if len(rs) == 0 {
		return nil, errors.New("no age recipients")
	}
	return rs, nil
}

// Sealed is a sealed copy, in a temporary file, ready to Put. Close removes
// it.
type Sealed struct {
	Blob
	// PlainSize and PlainSHA256 are of what was sealed, to check it against
	// what was recorded.
	PlainSize   int64
	PlainSHA256 string
	f           *os.File
}

// Close removes the sealed copy.
func (s *Sealed) Close() error {
	return errors.Join(s.f.Close(), os.Remove(s.f.Name()))
}

// Seal encrypts r into a new file in dir, readable by its owner alone.
func (s Sealer) Seal(r io.Reader, dir string) (*Sealed, error) {
	if len(s.Recipients) == 0 {
		return nil, errors.New("seal: no age recipients")
	}
	f, err := os.CreateTemp(dir, ".seal-*.age")
	if err != nil {
		return nil, err
	}
	sealed := &Sealed{f: f}
	ok := false
	defer func() {
		if !ok {
			_ = sealed.Close()
		}
	}()
	sum, plain := sha256.New(), sha256.New()
	out := &countingWriter{w: io.MultiWriter(f, sum)}
	w, err := age.Encrypt(out, s.Recipients...)
	if err != nil {
		return nil, err
	}
	n, err := io.Copy(w, io.TeeReader(r, plain))
	if err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	sealed.Blob = Blob{Body: f, Size: out.n, SHA256: hex.EncodeToString(sum.Sum(nil))}
	sealed.PlainSize, sealed.PlainSHA256 = n, hex.EncodeToString(plain.Sum(nil))
	ok = true
	return sealed, nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
