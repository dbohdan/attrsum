package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pkg/xattr"
)

const (
	dirPerm  = 0o755
	filePerm = 0o644
	noPerm   = 0o000

	// futureSkew advances a file's mtime far enough to be unambiguously
	// newer than a previously recorded sumtime.
	futureSkew = 2 * time.Second
)

// skipIfNoXattr skips the test unless the filesystem under dir supports the
// extended-attribute namespace the program actually uses. Probing with the
// real checksumKey (rather than a user.* key) matters on Linux, where regular
// files only accept xattrs in the user.* namespace and the program's
// berlin.sneak.* keys yield "operation not supported".
func skipIfNoXattr(t *testing.T, dir string) {
	t.Helper()

	probe := filepath.Join(dir, "xattr-probe")

	err := os.WriteFile(probe, []byte("probe"), filePerm)
	if err != nil {
		t.Fatalf("write probe file: %v", err)
	}

	defer func() { _ = os.Remove(probe) }()

	err = xattr.Set(probe, checksumKey, []byte("1"))
	if err != nil {
		t.Skipf("skipping: xattr namespace %q not supported: %v", checksumKey, err)

		return
	}

	_ = xattr.Remove(probe, checksumKey)
}

func writeFile(t *testing.T, root, name, content string) string {
	t.Helper()

	p := filepath.Join(root, name)

	err := os.MkdirAll(filepath.Dir(p), dirPerm)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	err = os.WriteFile(p, []byte(content), filePerm)
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	return p
}

func newTestStats() *Stats {
	return &Stats{StartTime: time.Now()}
}

func TestSumAddAndUpdate(t *testing.T) {
	t.Parallel()

	opts := &options{}
	dir := t.TempDir()
	skipIfNoXattr(t, dir)

	f := writeFile(t, dir, "a.txt", "hello")

	err := processSumAdd(opts, dir, newTestStats(), nil)
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	_, err = xattr.Get(f, checksumKey)
	if err != nil {
		t.Fatalf("checksum missing: %v", err)
	}

	tsb, _ := xattr.Get(f, sumTimeKey)
	origTime, _ := time.Parse(time.RFC3339Nano, string(tsb))

	err = os.WriteFile(f, []byte(strings.ToUpper("hello")), filePerm)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	now := time.Now().Add(futureSkew)

	err = os.Chtimes(f, now, now)
	if err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	err = processSumUpdate(opts, dir, newTestStats(), nil)
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	tsb2, _ := xattr.Get(f, sumTimeKey)

	newTime, _ := time.Parse(time.RFC3339Nano, string(tsb2))
	if !newTime.After(origTime) {
		t.Fatalf("sumtime not updated")
	}
}

func TestProcessCheckIntegration(t *testing.T) {
	t.Parallel()

	opts := &options{}
	dir := t.TempDir()
	skipIfNoXattr(t, dir)

	writeFile(t, dir, "b.txt", "world")

	err := processSumAdd(opts, dir, newTestStats(), nil)
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	err = processCheck(opts, dir, false, newTestStats(), nil)
	if err != nil {
		t.Fatalf("check ok: %v", err)
	}

	f := filepath.Join(dir, "b.txt")

	err = os.WriteFile(f, []byte("corrupt"), filePerm)
	if err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	err = processCheck(opts, dir, false, newTestStats(), nil)
	if err == nil {
		t.Fatalf("expected mismatch error, got nil")
	}
}

func TestClearRemovesAttrs(t *testing.T) {
	t.Parallel()

	opts := &options{}
	dir := t.TempDir()
	skipIfNoXattr(t, dir)

	f := writeFile(t, dir, "c.txt", "data")

	err := processSumAdd(opts, dir, newTestStats(), nil)
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	err = processClear(opts, dir, newTestStats(), nil)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}

	_, err = xattr.Get(f, checksumKey)
	if err == nil {
		t.Fatalf("checksum still present after clear")
	}

	_, err = xattr.Get(f, sumTimeKey)
	if err == nil {
		t.Fatalf("sumtime still present after clear")
	}
}

func TestExcludeDotfilesAndPatterns(t *testing.T) {
	t.Parallel()

	opts := &options{
		excludeDotfiles: true,
		excludePatterns: []string{"*.me"},
	}
	dir := t.TempDir()
	skipIfNoXattr(t, dir)

	hidden := writeFile(t, dir, ".hidden", "dot")
	keep := writeFile(t, dir, "keep.txt", "keep")
	skip := writeFile(t, dir, "skip.me", "skip")

	err := processSumAdd(opts, dir, newTestStats(), nil)
	if err != nil {
		t.Fatalf("add with excludes: %v", err)
	}

	_, err = xattr.Get(keep, checksumKey)
	if err != nil {
		t.Fatalf("expected xattr on keep.txt: %v", err)
	}

	_, err = xattr.Get(hidden, checksumKey)
	if err == nil {
		t.Fatalf(".hidden should have been excluded")
	}

	_, err = xattr.Get(skip, checksumKey)
	if err == nil {
		t.Fatalf("skip.me should have been excluded")
	}
}

func TestSkipBrokenSymlink(t *testing.T) {
	t.Parallel()

	opts := &options{}
	dir := t.TempDir()
	skipIfNoXattr(t, dir)

	// Create a dangling symlink.
	link := filepath.Join(dir, "dangling.lnk")

	err := os.Symlink(filepath.Join(dir, "nonexistent.txt"), link)
	if err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// Should not error and should not create xattrs on link.
	err = processSumAdd(opts, dir, newTestStats(), nil)
	if err != nil {
		t.Fatalf("processSumAdd with symlink: %v", err)
	}

	_, err = xattr.Get(link, checksumKey)
	if err == nil {
		t.Fatalf("symlink should not have xattr")
	}
}

func TestPermissionErrors(t *testing.T) {
	t.Parallel()

	opts := &options{}
	dir := t.TempDir()
	skipIfNoXattr(t, dir)

	secret := writeFile(t, dir, "secret.txt", "data")

	err := os.Chmod(secret, noPerm)
	if err != nil {
		t.Fatalf("chmod: %v", err)
	}

	defer func() { _ = os.Chmod(secret, filePerm) }()

	err = processSumAdd(opts, dir, newTestStats(), nil)
	if err == nil {
		t.Fatalf("expected permission error, got nil")
	}

	err = processSumUpdate(opts, dir, newTestStats(), nil)
	if err == nil {
		t.Fatalf("expected permission error on update, got nil")
	}

	err = processCheck(opts, dir, false, newTestStats(), nil)
	if err == nil {
		t.Fatalf("expected permission error on check, got nil")
	}
}
