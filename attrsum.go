package main

import (
    "bytes"
    "crypto/sha256"
    "errors"
    "fmt"
    "io"
    "log"
    "os"
    "path/filepath"
    "strings"
    "time"

    base58 "github.com/mr-tron/base58/base58"
    "github.com/bmatcuk/doublestar/v4"
    "github.com/multiformats/go-multihash"
    "github.com/pkg/xattr"
    "github.com/spf13/cobra"
)

const (
    checksumKey = "berlin.sneak.app.attrsum.checksum"
    sumTimeKey  = "berlin.sneak.app.attrsum.sumtime"
)

var (
    verbose         bool
    excludePatterns []string
    excludeDotfiles bool
)

func main() {
    rootCmd := &cobra.Command{
        Use:   "attrsum",
        Short: "Compute and verify file checksums via xattrs",
    }
    rootCmd.SilenceUsage = true
    rootCmd.SilenceErrors = true

    rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "enable verbose output")
    rootCmd.PersistentFlags().StringArrayVar(&excludePatterns, "exclude", nil, "exclude files/directories matching pattern (rsync-style, repeatable)")
    rootCmd.PersistentFlags().BoolVar(&excludeDotfiles, "exclude-dotfiles", false, "exclude any file or directory whose name starts with '.'")

    rootCmd.AddCommand(newSumCmd())
    rootCmd.AddCommand(newCheckCmd())
    rootCmd.AddCommand(newClearCmd())

    if err := rootCmd.Execute(); err != nil {
        log.Fatal(err)
    }
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
        Use:   "add <dir>",
        Short: "Write checksums for files missing them",
        Args:  cobra.ExactArgs(1),
        RunE: func(_ *cobra.Command, a []string) error { return ProcessSumAdd(a[0]) },
    }

    upd := &cobra.Command{
        Use:   "update <dir>",
        Short: "Recalculate checksum when file newer than stored sumtime",
        Args:  cobra.ExactArgs(1),
        RunE: func(_ *cobra.Command, a []string) error { return ProcessSumUpdate(a[0]) },
    }

    cmd.AddCommand(add, upd)
    return cmd
}

func ProcessSumAdd(dir string) error {
    return walkAndProcess(dir, func(p string, _ os.FileInfo) error { return writeChecksumAndTime(p) })
}

func ProcessSumUpdate(dir string) error {
    return walkAndProcess(dir, func(p string, info os.FileInfo) error {
        t, err := readSumTime(p)
        if err != nil || info.ModTime().After(t) {
            return writeChecksumAndTime(p)
        }
        return nil
    })
}

func writeChecksumAndTime(path string) error {
    hash, err := fileMultihash(path)
    if err != nil {
        return err
    }
    if err := xattr.Set(path, checksumKey, hash); err != nil {
        return fmt.Errorf("set checksum attr: %w", err)
    }
    if verbose {
        fmt.Printf("%s %s written\n", path, hash)
    }

    ts := time.Now().UTC().Format(time.RFC3339Nano)
    if err := xattr.Set(path, sumTimeKey, []byte(ts)); err != nil {
        return fmt.Errorf("set sumtime attr: %w", err)
    }
    if verbose {
        fmt.Printf("%s %s written\n", path, ts)
    }
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
        Use:   "clear <dir>",
        Short: "Remove checksum xattrs from tree",
        Args:  cobra.ExactArgs(1),
        RunE: func(_ *cobra.Command, a []string) error { return ProcessClear(a[0]) },
    }
}

func ProcessClear(dir string) error {
    return walkAndProcess(dir, func(p string, _ os.FileInfo) error {
        for _, k := range []string{checksumKey, sumTimeKey} {
            v, err := xattr.Get(p, k)
            if err != nil {
                if errors.Is(err, xattr.ENOATTR) {
                    continue
                }
                return err
            }
            if verbose {
                fmt.Printf("%s %s removed\n", p, string(v))
            }
            if err := xattr.Remove(p, k); err != nil {
                return err
            }
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
        Use:   "check <dir>",
        Short: "Verify stored checksums",
        Args:  cobra.ExactArgs(1),
        RunE: func(_ *cobra.Command, a []string) error { return ProcessCheck(a[0], cont) },
    }
    cmd.Flags().BoolVar(&cont, "continue", false, "continue after errors and report each file")
    return cmd
}

func ProcessCheck(dir string, cont bool) error {
    fail := errors.New("verification failed")
    bad := false

    err := walkAndProcess(dir, func(p string, _ os.FileInfo) error {
        exp, err := xattr.Get(p, checksumKey)
        if err != nil {
            if errors.Is(err, xattr.ENOATTR) {
                bad = true
                if verbose {
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
            return err
        }
        ok := bytes.Equal(exp, act)
        if !ok {
            bad = true
        }
        if verbose {
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
    if bad {
        return fail
    }
    return nil
}

///////////////////////////////////////////////////////////////////////////////
// Helpers
///////////////////////////////////////////////////////////////////////////////

func walkAndProcess(root string, fn func(string, os.FileInfo) error) error {
    root = filepath.Clean(root)
    return filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
        if err != nil {
            return err
        }

        // skip symlinks entirely
        if info.Mode()&os.ModeSymlink != 0 {
            if verbose {
                log.Printf("skip symlink %s", p)
            }
            if info.IsDir() {
                return filepath.SkipDir
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
            if verbose {
                log.Printf("skip non-regular %s", p)
            }
            return nil
        }
        return fn(p, info)
    })
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
