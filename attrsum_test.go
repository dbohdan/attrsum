package main

import (
	"errors"
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

	err = processCheck(opts, dir, newTestStats(), nil)
	if err != nil {
		t.Fatalf("check ok: %v", err)
	}

	f := filepath.Join(dir, "b.txt")

	err = os.WriteFile(f, []byte("corrupt"), filePerm)
	if err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	err = processCheck(opts, dir, newTestStats(), nil)
	if !errors.Is(err, errVerification) {
		t.Fatalf("expected verification error, got %v", err)
	}
}

// TestCheckSkipsUnchecksummed verifies that a file without a stored checksum
// is skipped rather than counted as a failure.
func TestCheckSkipsUnchecksummed(t *testing.T) {
	t.Parallel()

	opts := &options{}
	dir := t.TempDir()
	skipIfNoXattr(t, dir)

	writeFile(t, dir, "summed.txt", "summed")

	err := processSumAdd(opts, dir, newTestStats(), nil)
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	writeFile(t, dir, "fresh.txt", "never summed")

	stats := newTestStats()

	err = processCheck(opts, dir, stats, nil)
	if err != nil {
		t.Fatalf("check with unchecksummed file: %v", err)
	}

	if stats.FilesProcessed != 1 || stats.FilesSkipped != 1 || stats.FilesFailed != 0 {
		t.Fatalf("stats = processed %d skipped %d failed %d, want 1/1/0",
			stats.FilesProcessed, stats.FilesSkipped, stats.FilesFailed)
	}

	err = verificationResult(stats)
	if err != nil {
		t.Fatalf("verificationResult with no failures: %v", err)
	}
}

// TestCheckContinuesAfterMismatch verifies that a mismatch does not stop the
// scan: every file is still examined and the failure count drives the
// final error.
func TestCheckContinuesAfterMismatch(t *testing.T) {
	t.Parallel()

	opts := &options{}
	dir := t.TempDir()
	skipIfNoXattr(t, dir)

	first := writeFile(t, dir, "a/first.txt", "first")
	writeFile(t, dir, "b/second.txt", "second")
	writeFile(t, dir, "c/third.txt", "third")

	err := processSumAdd(opts, dir, newTestStats(), nil)
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	err = os.WriteFile(first, []byte("corrupt"), filePerm)
	if err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	stats := newTestStats()

	err = processCheck(opts, dir, stats, nil)
	if !errors.Is(err, errVerification) {
		t.Fatalf("expected verification error, got %v", err)
	}

	if stats.FilesProcessed != 2 || stats.FilesFailed != 1 {
		t.Fatalf("stats = processed %d failed %d, want 2/1 (scan did not continue)",
			stats.FilesProcessed, stats.FilesFailed)
	}

	err = verificationResult(stats)
	if !errors.Is(err, errVerification) {
		t.Fatalf("verificationResult with failures = %v, want errVerification", err)
	}

	if !strings.Contains(err.Error(), "1 file") {
		t.Fatalf("verificationResult message %q should carry the failure count", err)
	}
}

func TestIsDotfile(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		".":          false,
		"..":         false,
		"":           false,
		"plain":      false,
		"a.b":        false,
		".hidden":    true,
		"..twodots":  true,
		".DS_Store":  true,
		".Spotlight": true,
	}

	for name, want := range cases {
		if got := isDotfile(name); got != want {
			t.Errorf("isDotfile(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestShouldExcludeDotRoots verifies that the "." and ".." directory
// references are never treated as dotfiles, so passing them (or paths
// through them) as roots under --exclude-dotfiles still scans the tree.
func TestShouldExcludeDotRoots(t *testing.T) {
	t.Parallel()

	opts := &options{excludeDotfiles: true}

	for _, rel := range []string{".", "..", "", "dir/file.txt", filepath.Join("..", "x")} {
		if shouldExclude(opts, rel) {
			t.Errorf("shouldExclude(%q) = true, want false", rel)
		}
	}

	for _, rel := range []string{".hidden", filepath.Join("dir", ".hidden"), filepath.Join(".hidden", "x")} {
		if !shouldExclude(opts, rel) {
			t.Errorf("shouldExclude(%q) = false, want true", rel)
		}
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

// TestExcludedUnreadableDir covers the case of an unreadable directory
// (such as .Spotlight-V100 on a macOS volume) that the exclusion rules would
// prune. filepath.Walk reports the readdir error before the callback can
// evaluate exclusions, so the walker must apply them to errored entries too.
func TestExcludedUnreadableDir(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("skipping: root bypasses directory permissions")
	}

	dir := t.TempDir()
	skipIfNoXattr(t, dir)

	keep := writeFile(t, dir, "keep.txt", "keep")
	locked := filepath.Join(dir, ".locked")

	err := os.Mkdir(locked, noPerm)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	defer func() { _ = os.Chmod(locked, dirPerm) }()

	// Without the exclusion the unreadable directory is a hard error.
	err = processSumAdd(&options{}, dir, newTestStats(), nil)
	if err == nil {
		t.Fatalf("expected permission error without exclusion, got nil")
	}

	// With the exclusion the directory is pruned and the walk succeeds.
	opts := &options{excludeDotfiles: true}

	err = processSumAdd(opts, dir, newTestStats(), nil)
	if err != nil {
		t.Fatalf("add with excluded unreadable dir: %v", err)
	}

	_, err = countFiles(opts, dir)
	if err != nil {
		t.Fatalf("count with excluded unreadable dir: %v", err)
	}

	_, err = xattr.Get(keep, checksumKey)
	if err != nil {
		t.Fatalf("expected xattr on keep.txt: %v", err)
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

	err = processCheck(opts, dir, newTestStats(), nil)
	if err == nil {
		t.Fatalf("expected permission error on check, got nil")
	}
}
