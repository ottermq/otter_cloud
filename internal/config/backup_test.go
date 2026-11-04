package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testRecipient = "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p"

func recipientsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "recipients")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad_BackupNoneByDefault(t *testing.T) {
	cfg, err := Load(env(nil), Needs{Backup: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backup.Target != "none" || cfg.Backup.Name != "none" {
		t.Errorf("Backup = %+v, want none", cfg.Backup)
	}
}

func privateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoad_BackupFS(t *testing.T) {
	dir := privateDir(t)
	cfg, err := Load(env(map[string]string{
		EnvBackupTarget: "fs", EnvBackupTargetName: "interim", EnvBackupFSDir: dir,
		EnvBackupRecipientsFile: recipientsFile(t, "# offline key\n"+testRecipient+"\n"),
		EnvBackupMaxBytes:       "2G",
	}), Needs{Backup: true})
	if err != nil {
		t.Fatal(err)
	}
	b := cfg.Backup
	if b.Name != "interim" || b.FSDir != dir || b.MaxBytes != 2<<30 || !strings.Contains(b.Recipients, testRecipient) {
		t.Errorf("Backup = %+v", b)
	}
}

func TestLoad_BackupS3(t *testing.T) {
	creds := secretFile(t, 0o400, "AKID:the-secret-key\n")
	cfg, err := Load(env(map[string]string{
		EnvBackupTarget: "s3", EnvBackupS3URL: "https://s3.example.com/otter-backups/control-plane",
		EnvBackupS3CredentialsFile: creds, EnvBackupRecipientsFile: recipientsFile(t, testRecipient),
	}), Needs{Backup: true})
	if err != nil {
		t.Fatal(err)
	}
	b := cfg.Backup
	if b.S3Endpoint.String() != "https://s3.example.com" || b.S3Bucket != "otter-backups" || b.S3Prefix != "control-plane/" ||
		b.S3Region != "us-east-1" || b.S3AccessKey != "AKID" || b.S3SecretKey.Reveal() != "the-secret-key" || b.Name != "s3" {
		t.Errorf("Backup = %+v", b)
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("config", "config", cfg)
	if strings.Contains(buf.String(), "the-secret-key") || !strings.Contains(buf.String(), "otter-backups") {
		t.Errorf("logged: %s", buf.String())
	}
}

func TestLoad_BackupInvalid(t *testing.T) {
	dir := privateDir(t)
	open := t.TempDir()
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	recipients := recipientsFile(t, testRecipient)
	creds := secretFile(t, 0o400, "AKID:secret")
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"unknown target":     {map[string]string{EnvBackupTarget: "tape"}, EnvBackupTarget},
		"local name":         {map[string]string{EnvBackupTargetName: "local"}, EnvBackupTargetName},
		"no recipients":      {map[string]string{EnvBackupTarget: "fs", EnvBackupFSDir: dir}, EnvBackupRecipientsFile},
		"a private key":      {map[string]string{EnvBackupTarget: "fs", EnvBackupFSDir: dir, EnvBackupRecipientsFile: recipientsFile(t, "AGE-SECRET-KEY-1QQQ")}, "private key"},
		"no fs dir":          {map[string]string{EnvBackupTarget: "fs", EnvBackupRecipientsFile: recipients}, EnvBackupFSDir},
		"an open fs dir":     {map[string]string{EnvBackupTarget: "fs", EnvBackupFSDir: open, EnvBackupRecipientsFile: recipients}, "chmod 0700"},
		"a bad size":         {map[string]string{EnvBackupTarget: "fs", EnvBackupFSDir: dir, EnvBackupRecipientsFile: recipients, EnvBackupMaxBytes: "10GB"}, EnvBackupMaxBytes},
		"http s3":            {map[string]string{EnvBackupTarget: "s3", EnvBackupS3URL: "http://s3.example.com/b", EnvBackupS3CredentialsFile: creds, EnvBackupRecipientsFile: recipients}, "https://"},
		"s3 with userinfo":   {map[string]string{EnvBackupTarget: "s3", EnvBackupS3URL: "https://k:s@s3.example.com/b", EnvBackupS3CredentialsFile: creds, EnvBackupRecipientsFile: recipients}, "without credentials"},
		"s3 without bucket":  {map[string]string{EnvBackupTarget: "s3", EnvBackupS3URL: "https://s3.example.com/", EnvBackupS3CredentialsFile: creds, EnvBackupRecipientsFile: recipients}, "bucket"},
		"s3 no credentials":  {map[string]string{EnvBackupTarget: "s3", EnvBackupS3URL: "https://s3.example.com/b", EnvBackupRecipientsFile: recipients}, EnvBackupS3CredentialsFile},
		"s3 bad credentials": {map[string]string{EnvBackupTarget: "s3", EnvBackupS3URL: "https://s3.example.com/b", EnvBackupS3CredentialsFile: secretFile(t, 0o400, "no colon"), EnvBackupRecipientsFile: recipients}, "<access key id>"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(env(tc.env), Needs{Backup: true})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

func TestLoad_BackupS3OnLoopback(t *testing.T) {
	_, err := Load(env(map[string]string{
		EnvBackupTarget: "s3", EnvBackupS3URL: "http://127.0.0.1:9000/backups",
		EnvBackupS3CredentialsFile: secretFile(t, 0o400, "AKID:secret"), EnvBackupRecipientsFile: recipientsFile(t, testRecipient),
	}), Needs{Backup: true})
	if err != nil {
		t.Errorf("http on loopback: %v", err)
	}
}
