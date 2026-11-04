package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"filippo.io/age"

	"github.com/ottermq/otter_cloud/internal/backup"
	"github.com/ottermq/otter_cloud/internal/config"
)

// runBackup is otter backup: the backup target from outside the worker
// (#118). push copies the platform DB's dumps, which no worker makes; list
// and get are for restores. Each reads the OTTER_BACKUP_* settings it is
// run with.
func runBackup(args []string, stdout, stderr io.Writer) int {
	subs := map[string]func([]string, io.Writer, io.Writer) int{
		"push": runBackupPush, "list": runBackupList, "get": runBackupGet,
	}
	if len(args) == 0 || subs[args[0]] == nil {
		fmt.Fprintln(stderr, "Usage: otter backup <push|list|get> [flags]")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "  push  seal the dump files in a directory and copy the new ones to the target; keep the newest there")
		fmt.Fprintln(stderr, "  list  list what the target holds")
		fmt.Fprintln(stderr, "  get   fetch one copy and unseal it with an age identity kept off the node")
		if len(args) == 0 {
			return 2
		}
		switch args[0] {
		case "-h", "-help", "--help", "help":
			return 0
		}
		return 2
	}
	return subs[args[0]](args[1:], stdout, stderr)
}

// openTarget loads the settings and the target they name; none is an error
// here, as there is nothing to do.
func openTarget(name string, stderr io.Writer) (backup.Target, backup.Sealer, config.Backup, bool) {
	cfg, ok := loadConfig(name, config.Needs{Backup: true}, stderr)
	if !ok {
		return nil, backup.Sealer{}, config.Backup{}, false
	}
	target, sealer, err := backup.New(cfg.Backup)
	if err != nil {
		fmt.Fprintf(stderr, "otter %s: %s: %v\n", name, config.EnvBackupRecipientsFile, err)
		return nil, backup.Sealer{}, config.Backup{}, false
	}
	if target == nil {
		fmt.Fprintf(stderr, "otter %s: %s is none: there is no target\n", name, config.EnvBackupTarget)
		return nil, backup.Sealer{}, config.Backup{}, false
	}
	return target, sealer, cfg.Backup, true
}

func runBackupPush(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("backup push", "Seal the *.dump files in -dir that the target hasn't got and copy them under -prefix; then keep the newest -keep copies there", stderr)
	dir := fs.String("dir", "", "the directory of dump files (required)")
	prefix := fs.String("prefix", "platform/", "where the copies go on the target")
	keep := fs.Int("keep", 14, "how many copies to keep under -prefix, newest by name")
	if code, done := parse(fs, args); done {
		return code
	}
	if *dir == "" || *keep < 1 || !strings.HasSuffix(*prefix, "/") || backup.CheckKey(strings.TrimSuffix(*prefix, "/")) != nil {
		fmt.Fprintln(stderr, "otter backup push: -dir is required, -keep at least 1, and -prefix a key ending in /")
		return 2
	}
	target, sealer, cfg, ok := openTarget("backup push", stderr)
	if !ok {
		return 1
	}
	if err := push(context.Background(), target, sealer, cfg.MaxBytes, *dir, *prefix, *keep, stdout); err != nil {
		fmt.Fprintf(stderr, "otter backup push: %v\n", err)
		return 1
	}
	return 0
}

// push copies the newest keep dumps in dir that the target hasn't got,
// oldest first, then removes copies past the newest keep; names sort by
// time. A copy that would take what is under prefix past maxBytes isn't
// made. Nothing is removed if a copy failed.
func push(ctx context.Context, target backup.Target, sealer backup.Sealer, maxBytes int64, dir, prefix string, keep int, stdout io.Writer) error {
	have, err := target.List(ctx, prefix)
	if err != nil {
		return err
	}
	var held int64
	keys := map[string]bool{}
	for _, o := range have {
		held += o.Size
		keys[o.Key] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var dumps []string
	for _, e := range entries {
		if name := e.Name(); e.Type().IsRegular() && strings.HasSuffix(name, ".dump") && !strings.HasPrefix(name, ".") {
			dumps = append(dumps, name)
		}
	}
	slices.Sort(dumps)
	for _, name := range dumps[max(0, len(dumps)-keep):] {
		key := prefix + name + ".age"
		if keys[key] {
			continue
		}
		size, err := pushOne(ctx, target, sealer, filepath.Join(dir, name), key, maxBytes, held)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		held += size
		fmt.Fprintf(stdout, "copied %s (%d bytes)\n", key, size)
	}

	have, err = target.List(ctx, prefix)
	if err != nil {
		return err
	}
	var copies []string
	for _, o := range have {
		if strings.HasSuffix(o.Key, ".dump.age") {
			copies = append(copies, o.Key)
		}
	}
	slices.Sort(copies)
	for _, key := range copies[:max(0, len(copies)-keep)] {
		if err := target.Delete(ctx, key); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "removed %s\n", key)
	}
	return nil
}

func pushOne(ctx context.Context, target backup.Target, sealer backup.Sealer, path, key string, maxBytes, held int64) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sealed, err := sealer.Seal(f, os.TempDir())
	if err != nil {
		return 0, err
	}
	defer sealed.Close()
	if maxBytes > 0 && held+sealed.Size > maxBytes {
		return 0, fmt.Errorf("%d bytes would take the copies to %d, over %s's %d", sealed.Size, held+sealed.Size, config.EnvBackupMaxBytes, maxBytes)
	}
	if err := target.Put(ctx, key, sealed.Blob); err != nil {
		return 0, err
	}
	return sealed.Size, nil
}

func runBackupList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("backup list", "List the copies on the target, with their sizes in bytes", stderr)
	prefix := fs.String("prefix", "", "only keys starting with this")
	if code, done := parse(fs, args); done {
		return code
	}
	target, _, _, ok := openTarget("backup list", stderr)
	if !ok {
		return 1
	}
	objects, err := target.List(context.Background(), *prefix)
	if err != nil {
		fmt.Fprintf(stderr, "otter backup list: %v\n", err)
		return 1
	}
	for _, o := range objects {
		fmt.Fprintf(stdout, "%s\t%d\n", o.Key, o.Size)
	}
	return 0
}

func runBackupGet(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("backup get", "Fetch the copy at -key and unseal it into -o with the age identities in -identity", stderr)
	key := fs.String("key", "", "the copy's key, as backup list shows it (required)")
	identity := fs.String("identity", "", "a file of age identities (private keys), kept off the node (required)")
	out := fs.String("o", "", "the file to write; it must not exist (required)")
	if code, done := parse(fs, args); done {
		return code
	}
	if *key == "" || *identity == "" || *out == "" {
		fmt.Fprintln(stderr, "otter backup get: -key, -identity and -o are required")
		return 2
	}
	target, _, _, ok := openTarget("backup get", stderr)
	if !ok {
		return 1
	}
	if err := get(context.Background(), target, *key, *identity, *out); err != nil {
		fmt.Fprintf(stderr, "otter backup get: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote %s\n", *out)
	return 0
}

// get unseals the copy at key into out, which appears only whole: age
// refuses a copy that was changed or cut short.
func get(ctx context.Context, target backup.Target, key, identityFile, out string) error {
	idf, err := os.Open(identityFile)
	if err != nil {
		return err
	}
	ids, err := age.ParseIdentities(idf)
	idf.Close()
	if err != nil {
		return fmt.Errorf("%s: %w", identityFile, err)
	}
	if _, err := os.Lstat(out); err == nil {
		return fmt.Errorf("%s exists", out)
	}
	r, err := target.Get(ctx, key)
	if err != nil {
		return err
	}
	defer r.Close()
	plain, err := age.Decrypt(r, ids...)
	if err != nil {
		return fmt.Errorf("unseal %s: %w", key, err)
	}
	tmp := out + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp) // gone once renamed
	if _, err := io.Copy(f, plain); err != nil {
		f.Close()
		return fmt.Errorf("unseal %s: %w", key, err)
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	return os.Rename(tmp, out)
}
