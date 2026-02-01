[**attrsum**](https://git.eeqj.de/sneak/attrsum/) is a **Go
1.22** command-line utility that **adds, updates, verifies, and clears per-file
file content checksums stored in extended attributes (xattrs) on macOS (APFS) and
Linux**, released under the [WTFPL v2](http://www.wtfpl.net/).

Original release 2025-05-08.

Current version 1.0 (2025-05-08).

---

## Getting Started — Quick Build

```bash
# prerequisites: Go 1.22+
git clone https://git.eeqj.de/sneak/attrsum.git
cd attrsum
go build -o attrsum .
```

### Install

```bash
go install git.eeqj.de/sneak/attrsum@latest   # into GOPATH/bin or $(go env GOBIN)
```

Semantic Versioning 2.0.0 is used for tags.

---

## Usage

```bash
# add checksum & timestamp xattrs to every regular file under one or more paths
attrsum sum add DIR1 DIR2 file.txt

# update checksum only when file mtime is newer than stored sumtime
attrsum sum update DIR1 DIR2

# verify checksums, stop on first error
attrsum check DIR1 DIR2

# verify every file, reporting each result, keep going after errors
attrsum -v check --continue DIR1 DIR2

# remove checksum & timestamp xattrs
attrsum clear DIR1 DIR2

# read paths from stdin (use - as argument)
find /data -name "*.jpg" | attrsum sum add -

# quiet mode (suppress progress bar and summary)
attrsum -q sum add DIR
```

| xattr key                                   | meaning                        |
|---------------------------------------------|--------------------------------|
| `berlin.sneak.app.attrsum.checksum`         | base-58 multihash (sha2-256)   |
| `berlin.sneak.app.attrsum.sumtime`          | RFC 3339 timestamp of checksum |

Flags:

* `-v, --verbose` — per-file log output
* `-q, --quiet` — suppress all output except errors (no progress bar or summary)
* `--exclude PATTERN` — skip paths matching rsync/Doublestar glob
* `--exclude-dotfiles` — skip any path component that starts with `.`

All commands display a progress bar with ETA and print a summary report to stderr on completion (unless `--quiet` is specified).

`attrsum` **never follows symlinks** and skips non-regular files (sockets, devices, …).

---

## Why?

Apple APFS and Linux ext3/ext4 **store no per-file content checksums**, so
silent data corruption can pass unnoticed.  `attrsum` keeps a portable checksum **inside each file’s xattrs**, providing integrity
verification that travels with the file itself—no external database
required.  Now you can trust a USB stick didn't eat your data.

---

## TODO

Future improvements under consideration:

- **Dry-run mode (`--dry-run`, `-n`)** — show what would be done without making changes
- **JSON output (`--json`)** — machine-readable output for scripting and integration
- **Parallel processing (`-j N`)** — use multiple goroutines for faster checksumming on large trees
- **Exit code documentation** — formalize and document exit codes for scripting

---

## Contributing

* Author & maintainer: **sneak** – <sneak@sneak.berlin>
* Issues / PRs: <https://git.eeqj.de/sneak/attrsum/>
* Code must pass `go vet`, `go test ./...`, and `go fmt`.
* No CLA; contributions are under WTFPL v2.

---

## Community & Support

Bug tracker and wiki are in the Gitea repo linked above.

No formal Code of Conduct; be excellent to each other.

---

## License

*Everything is permitted.*
See [WTFPL v2](http://www.wtfpl.net/txt/copying/).
