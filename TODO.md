# Status

1.0+

Tagged 1.0.0 (2025-05-08). Substantial correctness fixes and features
have landed since the tag.

# Next Step

Policy scaffold commit: add LICENSE, REPO_POLICIES.md, .editorconfig,
.golangci.yml, and a comprehensive .gitignore (currently only the
attrsum binary), and extend the Makefile (only test/build/clean/try
today) with lint, fmt, fmt-check, check, and hooks targets. Fix the try
target to use a temp fixture instead of the hardcoded
$(HOME)/Documents/_SYSADMIN/cyberdyne path.

# Completed Steps

* 2026-02-02: correctness pass: track actual bytes read instead of
    stale file size, atomic failure tracking in ProcessCheck, detect
    file modification during checksum (TOCTOU), propagate countFiles
    errors, single progress bar across paths, error on empty stdin,
    dead code removal
* 2026-02-01: added quiet mode, progress bar, summary report, and stdin
    path input; multiple file/directory arguments for all commands
* 2025-07-12: README update
* 2025-05-08: initial working tool with passing tests, skips
    non-regular files, Makefile, README; tagged 1.0.0

# Future Steps

* Add Dockerfile and .dockerignore that run make check, images pinned
    by sha256, plus a Makefile docker target
* Add .gitea/workflows/check.yml
* Restructure README.md into the standard sections: Description,
    Getting Started, Rationale, Design, TODO, License, Author (Getting
    Started, Why?, TODO, License exist; Description, Design, Author are
    missing)
* Tag a patch release to ship the 2026-02-02 correctness fixes
* Dry-run mode (--dry-run, -n): show what would be done without making
    changes (from README TODO)
* JSON output (--json) for scripting and integration (from README TODO)
* Parallel processing (-j N) with multiple goroutines for faster
    checksumming on large trees (from README TODO)
* Formalize and document exit codes for scripting (from README TODO)
