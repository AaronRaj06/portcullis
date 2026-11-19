# ADR 0003: Keep every tool project-local; never touch the system Go

- Status: accepted
- Date: 2026-11-19

## Context

This machine's Go (1.27.4) comes from Homebrew and another project depends on it. `brew install kind vegeta nginx` can upgrade shared formulae, Go included, as a side effect.

## Decision

- No Homebrew installs and no global Go upgrade for this project.
- `kind` and `vegeta` are built with `go install` into `bin/tools/` (`make tools`, pinned versions). The Makefile puts `bin/tools` first on `PATH` for its own recipes only.
- NGINX, needed only as a benchmark baseline, runs as a pinned Docker image in Phase 4.
- The `go` line in `go.mod` is a per-module minimum. If a dependency ever needs a newer Go than the installed one, `GOTOOLCHAIN=auto` downloads that toolchain into the module cache for this module only, leaving `/opt/homebrew/bin/go` untouched.

## Consequences

- Fully isolated: deleting `bin/` removes every tool this project added.
- Tools are compiled from source on first install (about a minute).
