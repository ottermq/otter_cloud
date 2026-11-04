package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// FS is a Target in a directory: our own storage, such as a dataset that
// another machine pulls, or an NFS mount of its (architecture.md §10). Each
// key is a file under Dir, written to a temporary name and renamed, so a
// reader only ever sees whole blobs.
type FS struct {
	Dir string
}

func (t FS) path(key string) (string, error) {
	if err := CheckKey(key); err != nil {
		return "", err
	}
	return filepath.Join(t.Dir, filepath.FromSlash(key)), nil
}

func (t FS) Put(ctx context.Context, key string, b Blob) error {
	path, err := t.path(key)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+filepath.Base(path)+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp) // gone once renamed
	defer f.Close()
	if _, err := b.Body.Seek(0, io.SeekStart); err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), b.Body)
	if err != nil {
		return fmt.Errorf("write %s: %w", key, err)
	}
	if n != b.Size || hex.EncodeToString(h.Sum(nil)) != b.SHA256 {
		return fmt.Errorf("write %s: %d bytes, not the blob given", key, n)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func (t FS) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	path, err := t.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", key, ErrNotFound)
	}
	return f, err
}

func (t FS) List(ctx context.Context, prefix string) ([]Object, error) {
	if err := checkPrefix(prefix); err != nil {
		return nil, err
	}
	root := t.Dir
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		root = filepath.Join(t.Dir, filepath.FromSlash(prefix[:i]))
	}
	var out []Object
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && path == root {
			return fs.SkipAll
		}
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		rel, err := filepath.Rel(t.Dir, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) || CheckKey(key) != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, Object{Key: key, Size: info.Size()})
		return nil
	})
	slices.SortFunc(out, func(a, b Object) int { return strings.Compare(a.Key, b.Key) })
	return out, err
}

func (t FS) Delete(ctx context.Context, key string) error {
	path, err := t.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
