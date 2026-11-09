package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func blob(data string) Blob {
	sum := sha256.Sum256([]byte(data))
	return Blob{Body: strings.NewReader(data), Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
}

// testTarget is what every Target must do.
func testTarget(t *testing.T, target Target) {
	ctx := context.Background()
	for key, data := range map[string]string{
		"dumps/otc_0123456789ab-20261104T030000Z.dump.age": "first",
		"dumps/otc_0123456789ab-20261105T030000Z.dump.age": "second",
		"platform/otter-20261104T030100Z.dump.age":         "platform",
	} {
		if err := target.Put(ctx, key, blob(data)); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	// Put replaces.
	if err := target.Put(ctx, "dumps/otc_0123456789ab-20261105T030000Z.dump.age", blob("second, again")); err != nil {
		t.Fatal(err)
	}

	r, err := target.Get(ctx, "dumps/otc_0123456789ab-20261105T030000Z.dump.age")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil || string(got) != "second, again" {
		t.Errorf("get: %q, %v", got, err)
	}
	if _, err := target.Get(ctx, "dumps/missing.age"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get of a missing key: %v, want ErrNotFound", err)
	}

	objects, err := target.List(ctx, "dumps/")
	if err != nil {
		t.Fatal(err)
	}
	want := []Object{
		{"dumps/otc_0123456789ab-20261104T030000Z.dump.age", 5},
		{"dumps/otc_0123456789ab-20261105T030000Z.dump.age", 13},
	}
	if fmt.Sprint(objects) != fmt.Sprint(want) {
		t.Errorf("list dumps/: %v, want %v", objects, want)
	}
	if all, err := target.List(ctx, ""); err != nil || len(all) != 3 {
		t.Errorf("list all: %v, %v", all, err)
	}

	if err := target.Delete(ctx, "dumps/otc_0123456789ab-20261104T030000Z.dump.age"); err != nil {
		t.Fatal(err)
	}
	if err := target.Delete(ctx, "dumps/otc_0123456789ab-20261104T030000Z.dump.age"); err != nil {
		t.Errorf("delete of one gone: %v", err)
	}
	if objects, _ := target.List(ctx, "dumps/"); len(objects) != 1 {
		t.Errorf("after the delete: %v", objects)
	}

	for _, key := range []string{"../escape", "dumps/../../escape", "/abs", "dumps/.hidden", "a//b", "", "dumps/"} {
		if err := target.Put(ctx, key, blob("x")); err == nil {
			t.Errorf("put %q was accepted", key)
		}
	}
}

func TestFS(t *testing.T) {
	dir := t.TempDir()
	testTarget(t, FS{Dir: dir})
	// A blob that isn't what it says is refused, and leaves nothing.
	b := blob("short")
	b.Size = 6
	if err := (FS{Dir: dir}).Put(context.Background(), "dumps/bad.age", b); err == nil {
		t.Error("a blob of the wrong size was stored")
	}
	if _, err := os.Stat(filepath.Join(dir, "dumps", "bad.age")); err == nil {
		t.Error("the refused blob is there")
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "dumps")); len(entries) != 1 {
		t.Errorf("dumps/ holds %d entries, want the one copy and no temporary file", len(entries))
	}
}

func TestS3(t *testing.T) {
	testTarget(t, s3Server(t))
}

// The S3 call a wrong secret makes is refused: the server checks the
// signature.
func TestS3WrongSecret(t *testing.T) {
	target := s3Server(t)
	target.SecretKey = "not the secret"
	if err := target.Put(context.Background(), "dumps/x.age", blob("x")); err == nil {
		t.Error("put with the wrong secret succeeded")
	}
}

// AWS's example of a signed GET (Signature Version 4, "Example: GET
// Object" in the S3 API reference).
func TestS3Signature(t *testing.T) {
	target := &S3{Region: "us-east-1", AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	target.sign(req, emptySHA256, time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization:\n %s\nwant\n %s", got, want)
	}
}

// s3Image serves S3 over a directory, checking signatures: enough of the
// protocol for the four calls a Target makes.
const s3Image = "rclone/rclone:1.71"

// s3Server is an S3 target on a server of its own, in a container. Without
// docker the test is skipped, except in CI.
func s3Server(t *testing.T) *S3 {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("docker is needed for the S3 tests in CI")
		}
		t.Skip("docker is not installed")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	data := t.TempDir()
	if err := os.Mkdir(filepath.Join(data, "backups"), 0o700); err != nil {
		t.Fatal(err)
	}
	const access, secret = "otteraccesskey0001", "ottersecretkey00000000000000000000000001"
	name := fmt.Sprintf("otter-s3-test-%d", time.Now().UnixNano())
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name, "--network=host",
		"-u", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "-v", data+":/data", s3Image,
		"serve", "s3", "--auth-key", access+","+secret, "--addr", addr, "/data").CombinedOutput()
	if err != nil {
		t.Fatalf("start the S3 server: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "stop", name).Run() })
	target := &S3{Endpoint: &url.URL{Scheme: "http", Host: addr}, Bucket: "backups", Prefix: "node-a/",
		Region: "us-east-1", AccessKey: access, SecretKey: secret}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if _, err := target.List(context.Background(), ""); err == nil {
			break
		} else if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
			t.Fatalf("the S3 server didn't come up: %v\n%s", err, bytes.TrimSpace(logs))
		}
	}
	return target
}
