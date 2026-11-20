// Command attrsum computes and verifies file checksums stored in
// extended attributes.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	base58 "github.com/mr-tron/base58/base58"
	"github.com/multiformats/go-multihash"
	"github.com/pkg/xattr"
	"github.com/schollz/progressbar/v3"
	"github.com/spf13/cobra"
)

const (
	// The keys live in the user.* namespace so they are settable on Linux,
	// where regular-file xattrs outside user.*/trusted.*/security.*/system.*
	// are rejected with EOPNOTSUPP. macOS treats the whole string as an
	// opaque attribute name, so the same keys work there too.
	checksumKey = "user.berlin.sneak.app.attrsum.checksum"
	sumTimeKey  = "user.berlin.sneak.app.attrsum.sumtime"

	// progressThrottle is how often the progress bar repaints.
	progressThrottle = 250 * time.Millisecond
)

// Sentinel errors returned by the command implementations.
var (
	errNoPaths           = errors.New("no paths provided")
	errNoPathsStdin      = errors.New("no paths provided on stdin")
	errVerification      = errors.New("verification failed")
	errModifiedDuringSum = errors.New("file modified during checksum calculation")
)

// options holds the parsed command-line flag state that is shared across
// the sum/check/clear operations. It replaces package-level globals so the
// behaviour is explicit and the code is safe to exercise concurrently.
type options struct {
	verbose         bool
	quiet           bool
	excludePatterns []string
	excludeDotfiles bool
}

// Stats tracks operation statistics for summary reporting.
type Stats struct {
	FilesProcessed int64
	FilesSkipped   int64
	FilesFailed    int64
	BytesProcessed int64
	StartTime      time.Time
}

func (s *Stats) Duration() time.Duration {
	return time.Since(s.StartTime)
}

func (s *Stats) Print(opts *options, operation string) {
	if opts.quiet {
		return
	}

	fmt.Fprintf(os.Stderr, "\n%s complete: %d files processed, %d skipped, %d failed, %s bytes in %s\n",
		operation,
		s.FilesProcessed,
		s.FilesSkipped,
		s.FilesFailed,
		formatBytes(s.BytesProcessed),
		s.Duration().Round(time.Millisecond),
	)
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}

	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func main() {
	opts := &options{}

	rootCmd := &cobra.Command{
		Use:   "attrsum",
		Short: "Compute and verify file checksums via xattrs",
	}
	rootCmd.SilenceUsage = true
	rootCmd.SilenceErrors = true

	flags := rootCmd.PersistentFlags()
	flags.BoolVarP(&opts.verbose, "verbose", "v", false, "enable verbose output")
	flags.BoolVarP(&opts.quiet, "quiet", "q", false, "suppress all output except errors")
	flags.StringArrayVar(&opts.excludePatterns, "exclude", nil,
		"exclude files/directories matching pattern (rsync-style, repeatable)")
	flags.BoolVar(&opts.excludeDotfiles, "exclude-dotfiles", false,
		"exclude any file or directory whose name starts with '.'")

	rootCmd.AddCommand(newSumCmd(opts))
	rootCmd.AddCommand(newCheckCmd(opts))
	rootCmd.AddCommand(newClearCmd(opts))

	err := rootCmd.Execute()
	if err != nil {
		log.Fatal(err)
	}
}

// expandPaths expands the given paths, reading from stdin if "-" is present.
func expandPaths(args []string) ([]string, error) {
	var paths []string

	readFromStdin := false

	for _, arg := range args {
		if arg != "-" {
			paths = append(paths, arg)

			continue
		}

		readFromStdin = true
		scanner := bufio.NewScanner(os.Stdin)

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" {
				paths = append(paths, line)
			}
		}

		err := scanner.Err()
		if err != nil {
			return nil, fmt.Errorf("reading stdin: %w", err)
		}
	}

	if len(paths) == 0 {
		if readFromStdin {
			return nil, errNoPathsStdin
		}

		return nil, errNoPaths
	}

	return paths, nil
}

// processFunc processes a single path within a command's run loop.
type processFunc func(opts *options, path string, stats *Stats, prog *progress) error

// progress owns the progress bar and every line of per-file output printed
// while it is active. The bar lives on the terminal's last line; per-file
// lines are written above it by erasing the bar, printing the line to its
// own stream, and redrawing the bar immediately. The library's own redraw
// throttle is disabled so that redraw is never skipped; instead the
// per-file tick throttles itself. All methods are safe on a nil receiver
// and on a progress with no bar (quiet mode, or stderr not a terminal), in
// which case lines are printed directly.
type progress struct {
	bar      *progressbar.ProgressBar
	count    int64
	lastDraw time.Time
}

// newProgress counts the files under paths and returns a progress sized to
// that total. When the bar would not be shown (quiet mode, or stderr is not
// a terminal) no counting walk is performed and the returned progress only
// passes lines through.
func newProgress(opts *options, paths []string, desc string) (*progress, error) {
	if opts.quiet || !isTerminal(os.Stderr) {
		return &progress{}, nil
	}

	total, err := countFilesMultiple(opts, paths)
	if err != nil {
		return nil, err
	}

	return &progress{bar: newProgressBar(total, desc)}, nil
}

// isTerminal reports whether f is attached to a character device such as a
// terminal, as opposed to a pipe or regular file.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}

	return fi.Mode()&os.ModeCharDevice != 0
}

// tick records one more processed file, redrawing the bar at most once per
// progressThrottle.
func (p *progress) tick() {
	if p == nil || p.bar == nil {
		return
	}

	p.count++

	if time.Since(p.lastDraw) >= progressThrottle {
		p.draw()
	}
}

// draw repaints the bar with the current count.
func (p *progress) draw() {
	_ = p.bar.Set64(p.count)
	p.lastDraw = time.Now()
}

// printf writes one line of per-file output to w, keeping the bar on the
// last line of the terminal.
func (p *progress) printf(w io.Writer, format string, args ...any) {
	if p == nil || p.bar == nil {
		_, _ = fmt.Fprintf(w, format, args...)

		return
	}

	_ = p.bar.Clear()
	_, _ = fmt.Fprintf(w, format, args...)

	p.draw()
}

// finish completes the bar, leaving it rendered at 100% on its own line.
func (p *progress) finish() {
	if p == nil || p.bar == nil {
		return
	}

	_ = p.bar.Set64(p.count)
	_ = p.bar.Finish()
}

// runOverPaths runs process over each path, sharing the progress/stats
// bookkeeping common to the sum-add, sum-update and clear commands.
func runOverPaths(opts *options, args []string, desc, op string, process processFunc) error {
	paths, err := expandPaths(args)
	if err != nil {
		return err
	}

	stats := &Stats{StartTime: time.Now()}

	prog, err := newProgress(opts, paths, desc)
	if err != nil {
		return err
	}

	for _, p := range paths {
		perr := process(opts, p, stats, prog)
		if perr != nil {
			prog.finish()

			return perr
		}
	}

	prog.finish()
	stats.Print(opts, op)

	return nil
}

///////////////////////////////////////////////////////////////////////////////
// Sum commands
///////////////////////////////////////////////////////////////////////////////

func newSumCmd(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sum",
		Short: "Checksum maintenance operations",
	}

	add := &cobra.Command{
		Use:   "add <path>... (use - to read paths from stdin)",
		Short: "Write checksums for files missing them",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			return runOverPaths(opts, a, "Adding checksums", "sum add", processSumAdd)
		},
	}

	upd := &cobra.Command{
		Use:   "update <path>... (use - to read paths from stdin)",
		Short: "Recompute checksums where the file is newer than its sumtime, write missing ones",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			return runOverPaths(opts, a, "Updating checksums", "sum update", processSumUpdate)
		},
	}

	cmd.AddCommand(add, upd)

	return cmd
}

func processSumAdd(opts *options, dir string, stats *Stats, prog *progress) error {
	return walkAndProcess(opts, dir, stats, prog, func(p string, info os.FileInfo, s *Stats) error {
		if hasXattr(p, checksumKey) {
			atomic.AddInt64(&s.FilesSkipped, 1)

			return nil
		}

		err := writeChecksumAndTime(opts, p, info, s, prog)
		if err != nil {
			atomic.AddInt64(&s.FilesFailed, 1)

			return err
		}

		return nil
	})
}

func processSumUpdate(opts *options, dir string, stats *Stats, prog *progress) error {
	return walkAndProcess(opts, dir, stats, prog, func(p string, info os.FileInfo, s *Stats) error {
		t, err := readSumTime(p)
		if err != nil || info.ModTime().After(t) {
			werr := writeChecksumAndTime(opts, p, info, s, prog)
			if werr != nil {
				atomic.AddInt64(&s.FilesFailed, 1)

				return werr
			}

			return nil
		}

		atomic.AddInt64(&s.FilesSkipped, 1)

		return nil
	})
}

func writeChecksumAndTime(opts *options, path string, info os.FileInfo, stats *Stats, prog *progress) error {
	// Record mtime before hashing to detect modifications during hash.
	mtimeBefore := info.ModTime()

	hash, bytesRead, err := fileMultihash(path)
	if err != nil {
		return err
	}

	// Check if file was modified during hashing.
	infoAfter, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat after hash: %w", err)
	}

	if !infoAfter.ModTime().Equal(mtimeBefore) {
		return fmt.Errorf("%s: %w", path, errModifiedDuringSum)
	}

	err = xattr.Set(path, checksumKey, hash)
	if err != nil {
		return fmt.Errorf("set checksum attr: %w", err)
	}

	if opts.verbose && !opts.quiet {
		prog.printf(os.Stdout, "%s %s written\n", path, hash)
	}

	// Store the file's mtime as sumtime (not wall-clock time). This makes
	// update comparisons semantically correct.
	ts := mtimeBefore.UTC().Format(time.RFC3339Nano)

	err = xattr.Set(path, sumTimeKey, []byte(ts))
	if err != nil {
		return fmt.Errorf("set sumtime attr: %w", err)
	}

	if opts.verbose && !opts.quiet {
		prog.printf(os.Stdout, "%s %s written\n", path, ts)
	}

	atomic.AddInt64(&stats.FilesProcessed, 1)
	atomic.AddInt64(&stats.BytesProcessed, bytesRead)

	return nil
}

func readSumTime(path string) (time.Time, error) {
	b, err := xattr.Get(path, sumTimeKey)
	if err != nil {
		return time.Time{}, err
	}

	return time.Parse(time.RFC3339Nano, string(b))
}

///////////////////////////////////////////////////////////////////////////////
// Clear command
///////////////////////////////////////////////////////////////////////////////

func newClearCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "clear <path>... (use - to read paths from stdin)",
		Short: "Remove checksum xattrs from tree",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			return runOverPaths(opts, a, "Clearing checksums", "clear", processClear)
		},
	}
}

func processClear(opts *options, dir string, stats *Stats, prog *progress) error {
	return walkAndProcess(opts, dir, stats, prog, func(p string, info os.FileInfo, s *Stats) error {
		cleared, err := clearOne(opts, p, prog)
		if err != nil {
			atomic.AddInt64(&s.FilesFailed, 1)

			return err
		}

		if cleared {
			atomic.AddInt64(&s.FilesProcessed, 1)
			atomic.AddInt64(&s.BytesProcessed, info.Size())
		} else {
			atomic.AddInt64(&s.FilesSkipped, 1)
		}

		return nil
	})
}

// clearOne removes both checksum xattrs from a single path, reporting
// whether any attribute was actually removed.
func clearOne(opts *options, p string, prog *progress) (bool, error) {
	cleared := false

	for _, k := range []string{checksumKey, sumTimeKey} {
		v, err := xattr.Get(p, k)
		if err != nil {
			if errors.Is(err, xattr.ENOATTR) {
				continue
			}

			return cleared, err
		}

		if opts.verbose && !opts.quiet {
			prog.printf(os.Stdout, "%s %s removed\n", p, string(v))
		}

		rerr := xattr.Remove(p, k)
		if rerr != nil {
			return cleared, rerr
		}

		cleared = true
	}

	return cleared, nil
}

///////////////////////////////////////////////////////////////////////////////
// Check command
///////////////////////////////////////////////////////////////////////////////

func newCheckCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "check <path>... (use - to read paths from stdin)",
		Short: "Verify stored checksums",
		Long: `Verify stored checksums.

Every regular file under the given paths is scanned. Files that carry a
checksum are rehashed and compared; files without one are skipped. Each
mismatch or unreadable file is reported on stderr as it is found and the
scan continues. The exit status is non-zero when any file failed.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			return runCheck(opts, a)
		},
	}
}

func runCheck(opts *options, args []string) error {
	paths, err := expandPaths(args)
	if err != nil {
		return err
	}

	stats := &Stats{StartTime: time.Now()}

	prog, err := newProgress(opts, paths, "Verifying checksums")
	if err != nil {
		return err
	}

	for _, p := range paths {
		perr := processCheck(opts, p, stats, prog)
		if perr != nil && !errors.Is(perr, errVerification) {
			prog.finish()
			stats.Print(opts, "check")

			return perr
		}
	}

	prog.finish()
	stats.Print(opts, "check")

	return verificationResult(stats)
}

// verificationResult turns the accumulated failure count into the command's
// final error: nil when every checksummed file verified, otherwise
// errVerification wrapped with the count so the exit status is non-zero.
func verificationResult(stats *Stats) error {
	failed := atomic.LoadInt64(&stats.FilesFailed)
	if failed == 0 {
		return nil
	}

	return fmt.Errorf("%w: %d file(s)", errVerification, failed)
}

// processCheck verifies every file under dir. Per-file problems (mismatch,
// missing read permission) are reported and counted but never stop the
// walk; only structural walk errors abort it. The returned error is
// errVerification when any file under dir failed.
func processCheck(opts *options, dir string, stats *Stats, prog *progress) error {
	initialFailed := atomic.LoadInt64(&stats.FilesFailed)

	err := walkAndProcess(opts, dir, stats, prog, func(p string, _ os.FileInfo, s *Stats) error {
		res := checkOne(p, s)
		reportCheck(opts, prog, p, res)

		return nil
	})
	if err != nil {
		return err
	}

	if atomic.LoadInt64(&stats.FilesFailed) > initialFailed {
		return errVerification
	}

	return nil
}

// checkStatus classifies the outcome of verifying one file.
type checkStatus int

const (
	checkOK checkStatus = iota
	checkSkipped
	checkFailed
)

// checkResult is the outcome of verifying one file. detail is the computed
// multihash for a verified file, or the reason for a failure.
type checkResult struct {
	status checkStatus
	detail string
}

// checkOne verifies the stored checksum of a single path and records the
// outcome in s. It never returns an error: every outcome is a result so the
// caller can keep scanning.
func checkOne(p string, s *Stats) checkResult {
	exp, err := xattr.Get(p, checksumKey)
	if err != nil {
		if errors.Is(err, xattr.ENOATTR) {
			atomic.AddInt64(&s.FilesSkipped, 1)

			return checkResult{status: checkSkipped, detail: "no checksum"}
		}

		atomic.AddInt64(&s.FilesFailed, 1)

		return checkResult{status: checkFailed, detail: err.Error()}
	}

	act, bytesRead, err := fileMultihash(p)
	if err != nil {
		atomic.AddInt64(&s.FilesFailed, 1)

		return checkResult{status: checkFailed, detail: err.Error()}
	}

	if !bytes.Equal(exp, act) {
		atomic.AddInt64(&s.FilesFailed, 1)

		return checkResult{
			status: checkFailed,
			detail: fmt.Sprintf("checksum mismatch: stored %s, actual %s", exp, act),
		}
	}

	atomic.AddInt64(&s.FilesProcessed, 1)
	atomic.AddInt64(&s.BytesProcessed, bytesRead)

	return checkResult{status: checkOK, detail: string(act)}
}

// reportCheck prints a per-file verification result above the progress bar.
// Failures always go to stderr, even in quiet mode, because they are the
// errors the operator asked for. OK and skipped lines are printed only in
// verbose mode.
func reportCheck(opts *options, prog *progress, p string, res checkResult) {
	if res.status == checkFailed {
		prog.printf(os.Stderr, "%s ERROR %s\n", p, res.detail)

		return
	}

	if !opts.verbose || opts.quiet {
		return
	}

	switch res.status {
	case checkOK:
		prog.printf(os.Stdout, "%s %s OK\n", p, res.detail)
	case checkSkipped:
		prog.printf(os.Stdout, "%s <none> SKIP\n", p)
	case checkFailed:
		// Handled above.
	}
}

///////////////////////////////////////////////////////////////////////////////
// Helpers
///////////////////////////////////////////////////////////////////////////////

// countFiles counts the total number of regular files that will be processed.
func countFiles(opts *options, root string) (int64, error) {
	var count int64

	root = filepath.Clean(root)

	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return walkErr(opts, root, p, info, err)
		}

		// Skip symlinks - note: filepath.Walk uses Lstat, so symlinks are
		// reported as ModeSymlink, never as directories. Walk doesn't follow them.
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}

		rel, _ := filepath.Rel(root, p)
		if shouldExclude(opts, rel) {
			if info.IsDir() {
				return filepath.SkipDir
			}

			return nil
		}

		if info.IsDir() {
			return nil
		}

		if info.Mode().IsRegular() {
			count++
		}

		return nil
	})

	return count, err
}

// countFilesMultiple counts files across multiple roots.
func countFilesMultiple(opts *options, roots []string) (int64, error) {
	var total int64

	for _, root := range roots {
		count, err := countFiles(opts, root)
		if err != nil {
			return total, err
		}

		total += count
	}

	return total, nil
}

// newProgressBar creates a new progress bar with standard options. The
// library's redraw throttle is set to zero because progress.tick throttles
// itself; that way a redraw requested after printing a line is never
// dropped.
func newProgressBar(total int64, description string) *progressbar.ProgressBar {
	return progressbar.NewOptions64(total,
		progressbar.OptionSetDescription(description),
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionShowCount(),
		progressbar.OptionShowIts(),
		progressbar.OptionSetItsString("files"),
		progressbar.OptionThrottle(0),
		progressbar.OptionShowElapsedTimeOnFinish(),
		progressbar.OptionSetPredictTime(true),
		progressbar.OptionFullWidth(),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "=",
			SaucerHead:    ">",
			SaucerPadding: " ",
			BarStart:      "[",
			BarEnd:        "]",
		}),
	)
}

func walkAndProcess(
	opts *options,
	root string,
	stats *Stats,
	prog *progress,
	fn func(string, os.FileInfo, *Stats) error,
) error {
	root = filepath.Clean(root)

	return filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return walkErr(opts, root, p, info, err)
		}

		skip, skipErr := walkSkip(opts, root, p, info, prog)
		if skip {
			return skipErr
		}

		fnErr := fn(p, info, stats)

		prog.tick()

		return fnErr
	})
}

// walkErr decides what a walk callback should return when filepath.Walk
// reports an error for p. Walk reads a directory's entries before invoking
// the callback for it, so an unreadable directory arrives here with the
// readdir error already attached and the exclusion rules have not yet had a
// chance to prune it. A directory that would be excluded anyway (for example
// .Spotlight-V100 under --exclude-dotfiles) is therefore skipped silently;
// every other error aborts the walk. info is nil when Lstat itself failed.
func walkErr(opts *options, root, p string, info os.FileInfo, err error) error {
	if info != nil && info.IsDir() {
		rel, _ := filepath.Rel(root, p)
		if shouldExclude(opts, rel) {
			return filepath.SkipDir
		}
	}

	return err
}

// walkSkip reports whether a walked entry should be skipped before the
// per-file callback runs. When skip is true, the returned error is what the
// walk callback should return (filepath.SkipDir to prune an excluded
// directory, otherwise nil). Symlinks, directories and non-regular files are
// never checksummed.
func walkSkip(opts *options, root, p string, info os.FileInfo, prog *progress) (bool, error) {
	// filepath.Walk uses Lstat, so symlinks are reported as ModeSymlink,
	// never as directories. Walk doesn't follow them.
	if info.Mode()&os.ModeSymlink != 0 {
		if opts.verbose && !opts.quiet {
			prog.printf(os.Stderr, "skip symlink %s\n", p)
		}

		return true, nil
	}

	rel, _ := filepath.Rel(root, p)
	if shouldExclude(opts, rel) {
		if info.IsDir() {
			return true, filepath.SkipDir
		}

		return true, nil
	}

	if info.IsDir() {
		return true, nil
	}

	if !info.Mode().IsRegular() {
		if opts.verbose && !opts.quiet {
			prog.printf(os.Stderr, "skip non-regular %s\n", p)
		}

		return true, nil
	}

	return false, nil
}

func shouldExclude(opts *options, rel string) bool {
	if rel == "." || rel == "" {
		return false
	}

	if opts.excludeDotfiles {
		for part := range strings.SplitSeq(rel, string(os.PathSeparator)) {
			if isDotfile(part) {
				return true
			}
		}
	}

	for _, pat := range opts.excludePatterns {
		if ok, _ := doublestar.PathMatch(pat, rel); ok {
			return true
		}
	}

	return false
}

// isDotfile reports whether a single path component is a hidden entry: it
// begins with a dot and is not the "." or ".." directory reference.
func isDotfile(name string) bool {
	return strings.HasPrefix(name, ".") && name != "." && name != ".."
}

func hasXattr(path, key string) bool {
	_, err := xattr.Get(path, key)

	return err == nil
}

func fileMultihash(path string) ([]byte, int64, error) {
	// The path is supplied by the operator as the tree to checksum; reading
	// it is the entire purpose of the tool.
	f, err := os.Open(path) //nolint:gosec // G304: operator-specified path is the intended input
	if err != nil {
		return nil, 0, err
	}

	defer func() { _ = f.Close() }()

	h := sha256.New()

	bytesRead, err := io.Copy(h, f)
	if err != nil {
		return nil, bytesRead, err
	}

	mh, err := multihash.Encode(h.Sum(nil), multihash.SHA2_256)
	if err != nil {
		return nil, bytesRead, err
	}

	return []byte(base58.Encode(mh)), bytesRead, nil
}
