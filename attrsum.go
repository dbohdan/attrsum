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
	checksumKey = "berlin.sneak.app.attrsum.checksum"
	sumTimeKey  = "berlin.sneak.app.attrsum.sumtime"
)

var (
	verbose         bool
	quiet           bool
	excludePatterns []string
	excludeDotfiles bool
)

// Stats tracks operation statistics for summary reporting
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

func (s *Stats) Print(operation string) {
	if quiet {
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
	rootCmd := &cobra.Command{
		Use:   "attrsum",
		Short: "Compute and verify file checksums via xattrs",
	}
	rootCmd.SilenceUsage = true
	rootCmd.SilenceErrors = true

	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "enable verbose output")
	rootCmd.PersistentFlags().BoolVarP(&quiet, "quiet", "q", false, "suppress all output except errors")
	rootCmd.PersistentFlags().StringArrayVar(&excludePatterns, "exclude", nil, "exclude files/directories matching pattern (rsync-style, repeatable)")
	rootCmd.PersistentFlags().BoolVar(&excludeDotfiles, "exclude-dotfiles", false, "exclude any file or directory whose name starts with '.'")

	rootCmd.AddCommand(newSumCmd())
	rootCmd.AddCommand(newCheckCmd())
	rootCmd.AddCommand(newClearCmd())

	if err := rootCmd.Execute(); err != nil {
		log.Fatal(err)
	}
}

// expandPaths expands the given paths, reading from stdin if "-" is present
func expandPaths(args []string) ([]string, error) {
	var paths []string
	readFromStdin := false
	for _, arg := range args {
		if arg == "-" {
			readFromStdin = true
			scanner := bufio.NewScanner(os.Stdin)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line != "" {
					paths = append(paths, line)
				}
			}
			if err := scanner.Err(); err != nil {
				return nil, fmt.Errorf("reading stdin: %w", err)
			}
		} else {
			paths = append(paths, arg)
		}
	}
	if len(paths) == 0 {
		if readFromStdin {
			return nil, errors.New("no paths provided on stdin")
		}
		return nil, errors.New("no paths provided")
	}
	return paths, nil
}

///////////////////////////////////////////////////////////////////////////////
// Sum commands
///////////////////////////////////////////////////////////////////////////////

func newSumCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sum",
		Short: "Checksum maintenance operations",
	}

	add := &cobra.Command{
		Use:   "add <path>... (use - to read paths from stdin)",
		Short: "Write checksums for files missing them",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			paths, err := expandPaths(a)
			if err != nil {
				return err
			}
			stats := &Stats{StartTime: time.Now()}
			var bar *progressbar.ProgressBar
			if !quiet {
				total, err := countFilesMultiple(paths)
				if err != nil {
					return err
				}
				bar = newProgressBar(total, "Adding checksums")
			}
			for _, p := range paths {
				if err := ProcessSumAdd(p, stats, bar); err != nil {
					if bar != nil {
						bar.Finish()
					}
					return err
				}
			}
			if bar != nil {
				bar.Finish()
			}
			stats.Print("sum add")
			return nil
		},
	}

	upd := &cobra.Command{
		Use:   "update <path>... (use - to read paths from stdin)",
		Short: "Recalculate checksum when file newer than stored sumtime",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			paths, err := expandPaths(a)
			if err != nil {
				return err
			}
			stats := &Stats{StartTime: time.Now()}
			var bar *progressbar.ProgressBar
			if !quiet {
				total, err := countFilesMultiple(paths)
				if err != nil {
					return err
				}
				bar = newProgressBar(total, "Updating checksums")
			}
			for _, p := range paths {
				if err := ProcessSumUpdate(p, stats, bar); err != nil {
					if bar != nil {
						bar.Finish()
					}
					return err
				}
			}
			if bar != nil {
				bar.Finish()
			}
			stats.Print("sum update")
			return nil
		},
	}

	cmd.AddCommand(add, upd)
	return cmd
}

func ProcessSumAdd(dir string, stats *Stats, bar *progressbar.ProgressBar) error {
	return walkAndProcess(dir, stats, bar, func(p string, info os.FileInfo, s *Stats) error {
		if hasXattr(p, checksumKey) {
			atomic.AddInt64(&s.FilesSkipped, 1)
			return nil
		}
		if err := writeChecksumAndTime(p, info, s); err != nil {
			atomic.AddInt64(&s.FilesFailed, 1)
			return err
		}
		return nil
	})
}

func ProcessSumUpdate(dir string, stats *Stats, bar *progressbar.ProgressBar) error {
	return walkAndProcess(dir, stats, bar, func(p string, info os.FileInfo, s *Stats) error {
		t, err := readSumTime(p)
		if err != nil || info.ModTime().After(t) {
			if err := writeChecksumAndTime(p, info, s); err != nil {
				atomic.AddInt64(&s.FilesFailed, 1)
				return err
			}
		} else {
			atomic.AddInt64(&s.FilesSkipped, 1)
		}
		return nil
	})
}

func writeChecksumAndTime(path string, info os.FileInfo, stats *Stats) error {
	// Record mtime before hashing to detect modifications during hash
	mtimeBefore := info.ModTime()

	hash, err := fileMultihash(path)
	if err != nil {
		return err
	}

	// Check if file was modified during hashing
	infoAfter, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat after hash: %w", err)
	}
	if !infoAfter.ModTime().Equal(mtimeBefore) {
		return fmt.Errorf("%s: file modified during checksum calculation", path)
	}

	if err := xattr.Set(path, checksumKey, hash); err != nil {
		return fmt.Errorf("set checksum attr: %w", err)
	}
	if verbose && !quiet {
		fmt.Printf("%s %s written\n", path, hash)
	}

	// Store the file's mtime as sumtime (not wall-clock time)
	// This makes update comparisons semantically correct
	ts := mtimeBefore.UTC().Format(time.RFC3339Nano)
	if err := xattr.Set(path, sumTimeKey, []byte(ts)); err != nil {
		return fmt.Errorf("set sumtime attr: %w", err)
	}
	if verbose && !quiet {
		fmt.Printf("%s %s written\n", path, ts)
	}

	atomic.AddInt64(&stats.FilesProcessed, 1)
	atomic.AddInt64(&stats.BytesProcessed, infoAfter.Size())
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

func newClearCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clear <path>... (use - to read paths from stdin)",
		Short: "Remove checksum xattrs from tree",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			paths, err := expandPaths(a)
			if err != nil {
				return err
			}
			stats := &Stats{StartTime: time.Now()}
			var bar *progressbar.ProgressBar
			if !quiet {
				total, err := countFilesMultiple(paths)
				if err != nil {
					return err
				}
				bar = newProgressBar(total, "Clearing checksums")
			}
			for _, p := range paths {
				if err := ProcessClear(p, stats, bar); err != nil {
					if bar != nil {
						bar.Finish()
					}
					return err
				}
			}
			if bar != nil {
				bar.Finish()
			}
			stats.Print("clear")
			return nil
		},
	}
}

func ProcessClear(dir string, stats *Stats, bar *progressbar.ProgressBar) error {
	return walkAndProcess(dir, stats, bar, func(p string, info os.FileInfo, s *Stats) error {
		cleared := false
		for _, k := range []string{checksumKey, sumTimeKey} {
			v, err := xattr.Get(p, k)
			if err != nil {
				if errors.Is(err, xattr.ENOATTR) {
					continue
				}
				atomic.AddInt64(&s.FilesFailed, 1)
				return err
			}
			if verbose && !quiet {
				fmt.Printf("%s %s removed\n", p, string(v))
			}
			if err := xattr.Remove(p, k); err != nil {
				atomic.AddInt64(&s.FilesFailed, 1)
				return err
			}
			cleared = true
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

///////////////////////////////////////////////////////////////////////////////
// Check command
///////////////////////////////////////////////////////////////////////////////

func newCheckCmd() *cobra.Command {
	var cont bool
	cmd := &cobra.Command{
		Use:   "check <path>... (use - to read paths from stdin)",
		Short: "Verify stored checksums",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			paths, err := expandPaths(a)
			if err != nil {
				return err
			}
			stats := &Stats{StartTime: time.Now()}
			var bar *progressbar.ProgressBar
			if !quiet {
				total, err := countFilesMultiple(paths)
				if err != nil {
					return err
				}
				bar = newProgressBar(total, "Verifying checksums")
			}
			var finalErr error
			for _, p := range paths {
				if err := ProcessCheck(p, cont, stats, bar); err != nil {
					if cont {
						finalErr = err
					} else {
						if bar != nil {
							bar.Finish()
						}
						stats.Print("check")
						return err
					}
				}
			}
			if bar != nil {
				bar.Finish()
			}
			stats.Print("check")
			return finalErr
		},
	}
	cmd.Flags().BoolVar(&cont, "continue", false, "continue after errors and report each file")
	return cmd
}

func ProcessCheck(dir string, cont bool, stats *Stats, bar *progressbar.ProgressBar) error {
	fail := errors.New("verification failed")
	// Track initial failed count to detect failures during this walk
	initialFailed := atomic.LoadInt64(&stats.FilesFailed)

	err := walkAndProcess(dir, stats, bar, func(p string, info os.FileInfo, s *Stats) error {
		exp, err := xattr.Get(p, checksumKey)
		if err != nil {
			if errors.Is(err, xattr.ENOATTR) {
				atomic.AddInt64(&s.FilesFailed, 1)
				if verbose && !quiet {
					fmt.Printf("%s <none> ERROR\n", p)
				}
				if cont {
					return nil
				}
				return fail
			}
			return err
		}

		act, err := fileMultihash(p)
		if err != nil {
			atomic.AddInt64(&s.FilesFailed, 1)
			return err
		}
		ok := bytes.Equal(exp, act)
		if !ok {
			atomic.AddInt64(&s.FilesFailed, 1)
		} else {
			atomic.AddInt64(&s.FilesProcessed, 1)
			atomic.AddInt64(&s.BytesProcessed, info.Size())
		}
		if verbose && !quiet {
			status := "OK"
			if !ok {
				status = "ERROR"
			}
			fmt.Printf("%s %s %s\n", p, act, status)
		}
		if !ok && !cont {
			return fail
		}
		return nil
	})

	if err != nil {
		if errors.Is(err, fail) {
			return fail
		}
		return err
	}
	// Check if any failures occurred during this walk
	if atomic.LoadInt64(&stats.FilesFailed) > initialFailed {
		return fail
	}
	return nil
}

///////////////////////////////////////////////////////////////////////////////
// Helpers
///////////////////////////////////////////////////////////////////////////////

// countFiles counts the total number of regular files that will be processed
func countFiles(root string) (int64, error) {
	var count int64
	root = filepath.Clean(root)
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Skip symlinks - note: filepath.Walk uses Lstat, so symlinks are
		// reported as ModeSymlink, never as directories. Walk doesn't follow them.
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if shouldExclude(rel, info) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		count++
		return nil
	})
	return count, err
}

// countFilesMultiple counts files across multiple roots
func countFilesMultiple(roots []string) (int64, error) {
	var total int64
	for _, root := range roots {
		count, err := countFiles(root)
		if err != nil {
			return total, err
		}
		total += count
	}
	return total, nil
}

// newProgressBar creates a new progress bar with standard options
func newProgressBar(total int64, description string) *progressbar.ProgressBar {
	return progressbar.NewOptions64(total,
		progressbar.OptionSetDescription(description),
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionShowCount(),
		progressbar.OptionShowIts(),
		progressbar.OptionSetItsString("files"),
		progressbar.OptionThrottle(250*time.Millisecond),
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

func walkAndProcess(root string, stats *Stats, bar *progressbar.ProgressBar, fn func(string, os.FileInfo, *Stats) error) error {
	root = filepath.Clean(root)

	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip symlinks - filepath.Walk uses Lstat, so symlinks are reported
		// as ModeSymlink, never as directories. Walk doesn't follow them.
		if info.Mode()&os.ModeSymlink != 0 {
			if verbose && !quiet {
				log.Printf("skip symlink %s", p)
			}
			return nil
		}

		rel, _ := filepath.Rel(root, p)
		if shouldExclude(rel, info) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			if verbose && !quiet {
				log.Printf("skip non-regular %s", p)
			}
			return nil
		}

		fnErr := fn(p, info, stats)
		if bar != nil {
			bar.Add(1)
		}
		return fnErr
	})

	return err
}

func shouldExclude(rel string, info os.FileInfo) bool {
	if rel == "." || rel == "" {
		return false
	}
	if excludeDotfiles {
		for _, part := range strings.Split(rel, string(os.PathSeparator)) {
			if strings.HasPrefix(part, ".") {
				return true
			}
		}
	}
	for _, pat := range excludePatterns {
		if ok, _ := doublestar.PathMatch(pat, rel); ok {
			return true
		}
	}
	return false
}

func hasXattr(path, key string) bool {
	_, err := xattr.Get(path, key)
	return err == nil
}

func fileMultihash(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	mh, err := multihash.Encode(h.Sum(nil), multihash.SHA2_256)
	if err != nil {
		return nil, err
	}
	return []byte(base58.Encode(mh)), nil
}
