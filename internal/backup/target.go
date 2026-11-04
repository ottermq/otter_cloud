package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
)

// Target stores opaque, already-encrypted blobs under keys the caller
// chooses (architecture.md §10, AD-11). Encryption and retention are above
// it: a target only stores, lists and deletes.
type Target interface {
	// Put stores b under key, replacing what was there. The object appears
	// whole or not at all.
	Put(ctx context.Context, key string, b Blob) error
	// Get returns the object under key, or ErrNotFound.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// List returns the objects whose keys start with prefix, in key order.
	List(ctx context.Context, prefix string) ([]Object, error)
	// Delete removes the object under key. One already gone is not an
	// error.
	Delete(ctx context.Context, key string) error
}

// Blob is what Put stores: its size and SHA-256 are known before it is
// sent, so a target can have them checked on the way.
type Blob struct {
	Body   io.ReadSeeker
	Size   int64
	SHA256 string // hex
}

// Object is a stored blob.
type Object struct {
	Key  string
	Size int64
}

// ErrNotFound means no object has the key.
var ErrNotFound = errors.New("no such backup object")

// A key is slash-separated names, none starting with a dot: it can't climb
// out of a target's directory or prefix, nor hit a temporary file.
var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_][A-Za-z0-9_.-]*)*$`)

// CheckKey reports whether key is one a target accepts.
func CheckKey(key string) error {
	if len(key) > 512 || !keyPattern.MatchString(key) {
		return fmt.Errorf("backup key %q: not slash-separated names of letters, digits, '.', '_' and '-'", key)
	}
	return nil
}

// checkPrefix reports whether prefix is empty or a key followed by a slash,
// or the start of a name.
func checkPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if p := trimSlash(prefix); CheckKey(p) != nil {
		return fmt.Errorf("backup prefix %q: not the start of a key", prefix)
	}
	return nil
}

func trimSlash(s string) string {
	if len(s) > 0 && s[len(s)-1] == '/' {
		return s[:len(s)-1]
	}
	return s
}
