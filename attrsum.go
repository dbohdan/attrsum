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
	"time"

	base58 "github.com/mr-tron/base58/base58"
	"github.com/multiformats/go-multihash"
	"github.com/pkg/xattr"
	"github.com/spf13/cobra"
)

const (
	// Extended-attribute keys
	checksumKey = "berlin.sneak.app.attrsum.checksum"
	sumTimeKey  = "berlin.sneak.app.attrsum.sumtime"
)

var verbose bool

func main() {
	rootCmd := &cobra.Command{
		Use:   "attrsum",
		Short: "Compute and verify file checksums via xattrs",
	}

	rootCmd.PersistentFlags().BoolVarP(
		&verbose, "verbose", "v", false, "enable verbose output",
	)

	rootCmd.AddCommand(newSumCmd())
	rootCmd.AddCommand(newCheckCmd())

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

// ProcessSumAdd writes checksum & sumtime only when checksum is absent.
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

// ProcessSumUpdate recalculates checksum when file mtime exceeds stored
// sumtime (or when attributes are missing / malformed).
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
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	if err := xattr.Set(path, sumTimeKey, []byte(nowStr)); err != nil {
		return fmt.Errorf("set sumtime attr: %w", err)
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

	cmd.Flags().BoolVar(&cont, "continue", false,
		"continue after errors and report each file")
	return cmd
}

// ProcessCheck verifies checksums and exits non-zero on first error unless
// --continue is supplied.
func ProcessCheck(dir string, cont bool) error {
	exitErr := errors.New("verification failed")

	err := walkAndProcess(dir, func(path string, info os.FileInfo) error {
		exp, err := xattr.Get(path, checksumKey)
		if err != nil {
			if errors.Is(err, xattr.ENOATTR) {
				log.Printf("ERROR missing xattr %s", path)
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

		if !bytes.Equal(exp, act) {
			log.Printf("ERROR checksum mismatch %s", path)
			if cont {
				return nil
			}
			return exitErr
		}
		if cont {
			fmt.Printf("OK %s\n", path)
		}
		return nil
	})

	if err != nil {
		if errors.Is(err, exitErr) {
			return exitErr
		}
		return err
	}
	return nil
}

///////////////////////////////////////////////////////////////////////////////
// Helpers
///////////////////////////////////////////////////////////////////////////////

func walkAndProcess(root string, fn func(string, os.FileInfo) error) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		return fn(path, info)
	})
}

func hasXattr(path, key string) bool {
	_, err := xattr.Get(path, key)
	return err == nil
}

// fileMultihash returns the base58-encoded SHA-2-256 multihash of the file.
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
