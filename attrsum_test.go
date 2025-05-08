package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pkg/xattr"
)

// skipIfNoXattr skips tests when the underlying FS lacks xattr support.
func skipIfNoXattr(t *testing.T, path string) {
	if err := xattr.Set(path, "user.test", []byte("1")); err != nil {
		t.Skipf("skipping: xattr not supported: %v", err)
	} else {
		_ = xattr.Remove(path, "user.test")
	}
}

func writeFile(t *testing.T, root, name, content string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestSumAddAndUpdate(t *testing.T) {
	dir := t.TempDir()
	skipIfNoXattr(t, dir)

	f := writeFile(t, dir, "a.txt", "hello")

	// Add: write missing checksum.
	if err := ProcessSumAdd(dir); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Attributes should exist.
	if _, err := xattr.Get(f, checksumKey); err != nil {
		t.Fatalf("checksum missing: %v", err)
	}
	tsb, err := xattr.Get(f, sumTimeKey)
	if err != nil {
		t.Fatalf("sumtime missing: %v", err)
	}
	origTime, _ := time.Parse(time.RFC3339Nano, string(tsb))

	// Modify file and bump mtime.
	if err := os.WriteFile(f, []byte(strings.ToUpper("hello")), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	now := time.Now().Add(2 * time.Second)
	os.Chtimes(f, now, now)

	// Update should refresh checksum and time.
	if err := ProcessSumUpdate(dir); err != nil {
		t.Fatalf("update: %v", err)
	}
	tsb2, _ := xattr.Get(f, sumTimeKey)
	newTime, _ := time.Parse(time.RFC3339Nano, string(tsb2))
	if !newTime.After(origTime) {
		t.Fatalf("sumtime not updated")
	}
}

func TestProcessCheckIntegration(t *testing.T) {
	dir := t.TempDir()
	skipIfNoXattr(t, dir)

	writeFile(t, dir, "b.txt", "world")
	if err := ProcessSumAdd(dir); err != nil {
		t.Fatalf("add: %v", err)
	}

	if err := ProcessCheck(dir, false); err != nil {
		t.Fatalf("check ok: %v", err)
	}

	// Corrupt file contents should produce an error.
	f := filepath.Join(dir, "b.txt")
	os.WriteFile(f, []byte("corrupt"), 0o644)

	if err := ProcessCheck(dir, false); err == nil {
		t.Fatalf("expected mismatch error, got nil")
	}
}

func TestPermissionErrors(t *testing.T) {
	dir := t.TempDir()
	skipIfNoXattr(t, dir)

	secret := writeFile(t, dir, "secret.txt", "data")
	os.Chmod(secret, 0o000)
	defer os.Chmod(secret, 0o644)

	if err := ProcessSumAdd(dir); err == nil {
		t.Fatalf("expected permission error, got nil")
	}
	if err := ProcessSumUpdate(dir); err == nil {
		t.Fatalf("expected permission error on update, got nil")
	}
	if err := ProcessCheck(dir, false); err == nil {
		t.Fatalf("expected permission error on check, got nil")
	}
}
