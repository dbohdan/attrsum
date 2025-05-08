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
// Sum operations
///////////////////////////////////////////////////////////////////////////////

func newSumCmd() *cobra.Command {
    cmd := &cobra.Command{
        Use:   "sum",
        Short: "Checksum maintenance operations",
    }

    addCmd := &cobra.Command{
        Use:   "add <directory>",
        Short: "Write checksums for files missing them",
        Args:  cobra.ExactArgs(1),
        RunE: func(_ *cobra.Command, args []string) error {
            return ProcessSumAdd(args[0])
        },
    }

    updateCmd := &cobra.Command{
        Use:   "update <directory>",
        Short: "Refresh checksums when file modified",
        Args:  cobra.ExactArgs(1),
        RunE: func(_ *cobra.Command, args []string) error {
            return ProcessSumUpdate(args[0])
        },
    }

    cmd.AddCommand(addCmd, updateCmd)
    return cmd
}

func ProcessSumAdd(dir string) error {
    return walkAndProcess(dir, func(path string, info os.FileInfo) error {
        if hasXattr(path, checksumKey) {
            if verbose {
                log.Printf("skip existing %s", path)
            }
            return nil
        }
        return writeChecksumAndTime(path)
    })
}

func ProcessSumUpdate(dir string) error {
    return walkAndProcess(dir, func(path string, info os.FileInfo) error {
        needUpdate := false
        t, err := readSumTime(path)
        if err != nil || info.ModTime().After(t) {
            needUpdate = true
        }
        if needUpdate {
            if verbose {
                log.Printf("update %s", path)
            }
            return writeChecksumAndTime(path)
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
        fmt.Printf("%s %s written\n", path, string(hash))
    }

    nowStr := time.Now().UTC().Format(time.RFC3339Nano)
    if err := xattr.Set(path, sumTimeKey, []byte(nowStr)); err != nil {
        return fmt.Errorf("set sumtime attr: %w", err)
    }
    if verbose {
        fmt.Printf("%s %s written\n", path, nowStr)
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
// Clear operation
///////////////////////////////////////////////////////////////////////////////

func newClearCmd() *cobra.Command {
    return &cobra.Command{
        Use:   "clear <directory>",
        Short: "Remove checksum xattrs from files in tree",
        Args:  cobra.ExactArgs(1),
        RunE: func(_ *cobra.Command, args []string) error {
            return ProcessClear(args[0])
        },
    }
}

func ProcessClear(dir string) error {
    return walkAndProcess(dir, func(path string, info os.FileInfo) error {
        for _, key := range []string{checksumKey, sumTimeKey} {
            val, err := xattr.Get(path, key)
            if err != nil {
                if errors.Is(err, xattr.ENOATTR) {
                    continue
                }
                return err
            }
            if verbose {
                fmt.Printf("%s %s removed\n", path, string(val))
            }
            if err := xattr.Remove(path, key); err != nil {
                return err
            }
        }
        return nil
    })
}

///////////////////////////////////////////////////////////////////////////////
// Check operation
///////////////////////////////////////////////////////////////////////////////

func newCheckCmd() *cobra.Command {
    var cont bool
    cmd := &cobra.Command{
        Use:   "check <directory>",
        Short: "Verify stored checksums against file contents",
        Args:  cobra.ExactArgs(1),
        RunE: func(_ *cobra.Command, args []string) error {
            return ProcessCheck(args[0], cont)
        },
    }
    cmd.Flags().BoolVar(&cont, "continue", false, "continue after errors and report each file")
    return cmd
}

func ProcessCheck(dir string, cont bool) error {
    exitErr := errors.New("verification failed")
    hadErr := false

    err := walkAndProcess(dir, func(path string, info os.FileInfo) error {
        exp, err := xattr.Get(path, checksumKey)
        if err != nil {
            if errors.Is(err, xattr.ENOATTR) {
                hadErr = true
                if verbose {
                    fmt.Printf("%s <none> ERROR\n", path)
                } else {
                    log.Printf("ERROR missing xattr %s", path)
                }
                if cont {
                    return nil
                }
                return exitErr
            }
            return err
        }

        act, err := fileMultihash(path)
        if err != nil {
            return err
        }

        ok := bytes.Equal(exp, act)
        if !ok {
            hadErr = true
        }

        if verbose {
            res := "OK"
            if !ok {
                res = "ERROR"
            }
            fmt.Printf("%s %s %s\n", path, string(act), res)
        } else if !ok {
            log.Printf("ERROR checksum mismatch %s", path)
        }

        if !ok && !cont {
            return exitErr
        }
        return nil
    })

    if err != nil {
        if errors.Is(err, exitErr) {
            return exitErr
        }
        return err
    }
    if hadErr {
        return exitErr
    }
    return nil
}

///////////////////////////////////////////////////////////////////////////////
// Helpers
///////////////////////////////////////////////////////////////////////////////

func walkAndProcess(root string, fn func(string, os.FileInfo) error) error {
    root = filepath.Clean(root)
    return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
        if err != nil {
            return err
        }
        rel, _ := filepath.Rel(root, path)

        if shouldExclude(rel, info) {
            if info.IsDir() {
                return filepath.SkipDir
            }
            return nil
        }

        if info.IsDir() {
            return nil
        }
        return fn(path, info)
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
