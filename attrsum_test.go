package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pkg/xattr"
)

// skipIfNoXattr skips tests when underlying FS lacks xattr support.
func skipIfNoXattr(t *testing.T, path string) {
	if err := xattr.Set(path, "user.test", []byte("1")); err != nil {
		t.Skipf("skipping: xattr not supported: %v", err)
	} else {
		_ = xattr.Remove(path, "user.test")
	}
}

func writeFile(t *testing.T, root, name, content string) string {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestSumAddAndUpdate(t *testing.T) {
	dir := t.TempDir()
	skipIfNoXattr(t, dir)

	f := writeFile(t, dir, "a.txt", "hello")

	if err := ProcessSumAdd(dir); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := xattr.Get(f, checksumKey); err != nil {
		t.Fatalf("checksum missing: %v", err)
	}
	tsb, _ := xattr.Get(f, sumTimeKey)
	origTime, _ := time.Parse(time.RFC3339Nano, string(tsb))

	// Modify file & bump mtime to force update.
	os.WriteFile(f, []byte(strings.ToUpper("hello")), 0o644)
	now := time.Now().Add(2 * time.Second)
	os.Chtimes(f, now, now)

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

	// Corrupt -> should fail
	f := filepath.Join(dir, "b.txt")
	os.WriteFile(f, []byte("corrupt"), 0o644)

	if err := ProcessCheck(dir, false); err == nil {
		t.Fatalf("expected mismatch error, got nil")
	}
}

func TestClearRemovesAttrs(t *testing.T) {
	dir := t.TempDir()
	skipIfNoXattr(t, dir)

	f := writeFile(t, dir, "c.txt", "data")
	if err := ProcessSumAdd(dir); err != nil {
		t.Fatalf("add: %v", err)
	}
	// Ensure attrs exist
	if _, err := xattr.Get(f, checksumKey); err != nil {
		t.Fatalf("pre-clear checksum missing: %v", err)
	}

	if err := ProcessClear(dir); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := xattr.Get(f, checksumKey); err == nil {
		t.Fatalf("checksum still present after clear")
	}
	if _, err := xattr.Get(f, sumTimeKey); err == nil {
		t.Fatalf("sumtime still present after clear")
	}
}

func TestExcludeDotfilesAndPatterns(t *testing.T) {
	dir := t.TempDir()
	skipIfNoXattr(t, dir)

	hidden := writeFile(t, dir, ".hidden", "dot")
	keep := writeFile(t, dir, "keep.txt", "keep")
	skip := writeFile(t, dir, "skip.me", "skip")

	// Save global state then set exclusions
	oldDotfiles := excludeDotfiles
	oldPatterns := excludePatterns
	excludeDotfiles = true
	excludePatterns = []string{"*.me"}

	defer func() {
		excludeDotfiles = oldDotfiles
		excludePatterns = oldPatterns
	}()

	if err := ProcessSumAdd(dir); err != nil {
		t.Fatalf("add with excludes: %v", err)
	}

	// keep.txt should have xattrs
	if _, err := xattr.Get(keep, checksumKey); err != nil {
		t.Fatalf("expected xattr on keep.txt: %v", err)
	}
	// .hidden and skip.me should not
	if _, err := xattr.Get(hidden, checksumKey); err == nil {
		t.Fatalf(".hidden should have been excluded")
	}
	if _, err := xattr.Get(skip, checksumKey); err == nil {
		t.Fatalf("skip.me should have been excluded by pattern")
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
