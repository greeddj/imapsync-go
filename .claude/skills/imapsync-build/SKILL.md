---
name: imapsync-build
description: Build imapsync-go binaries and OCI images via Justfile — host build, Linux amd64 build, container image. Use when the user asks to build a binary, produce a release artifact in dist/, or build a container image. Do NOT use for tests (see imapsync-test) or lint (see imapsync-check).
---

# imapsync-go — Build & OCI

## When to use

- "собери бинарь" / "build binary" / "release artifact"
- "собери под Linux" / "linux amd64"
- "собери образ" / "build image" / "OCI"

## Commands

| Intent | Command |
|---|---|
| Host build | `just build` |
| Linux amd64 build | `just build_linux` |
| OCI image (default podman, tag local) | `just oci` |
| OCI with explicit args | `just oci executor=podman tag=local` |
| OCI with docker | `just oci executor=docker tag=v1.2.3` |

## Outputs

- `just build` → `dist/imapsync-go` (host OS/arch)
- `just build_linux` → `dist/imapsync-go` (overwrites — Linux amd64)
- `just oci` → chains `build_linux`, then runs `<executor> build -f Dockerfile`. Image: `imapsync-go:<tag>`.

The Linux build target writes to the **same** `dist/imapsync-go` path as the host build — it overwrites. If you need both, rename or move between invocations.

## Build flags

Both build targets:

- `CGO_ENABLED=0`
- `-trimpath`
- `-ldflags="-s -w -X main.Version=<git-tag-or-branch> -X main.Commit=<short-sha> -X main.Date=<UTC-RFC3339> -X main.BuiltBy=just"`

`Version` resolves to the latest git tag, falling back to current branch name if no tag exists. `Commit` is `git rev-parse --short HEAD`. These four `main.*` vars are wired in `cmd/imapsync-go/main.go` and printed by `--version`.

## Pre-build chain

- `just build` depends on `just check + just lint + just test` — every host build runs the full static-analysis suite, lint, and tests first. `just check` itself depends on `just deps`, which is mutating (runs `go mod tidy && go mod vendor`).
- `just build_linux` depends only on `just check` (no lint/test). Still mutating via `deps`.

If the user wants a build *without* dep mutation, run `go build` directly with the same `-ldflags` and `CGO_ENABLED=0 -trimpath`, or warn before proceeding.

For release artifacts across all platforms, the project uses `goreleaser` via `.goreleaser.yml` — that's the source of truth for what ends up on GitHub Releases / Homebrew tap / GHCR. Don't hand-edit `dist/` for releases.

## OCI gotchas

- Requires a container runtime (`podman` default, `docker` works as alternative — pass via `executor=`).
- Builds the linux amd64 binary first; the `Dockerfile` consumes `dist/imapsync-go`.
- The image entrypoint expects a config mounted at runtime (see README — `-v .../config.json:/config.json`).

## Workflow

1. Confirm whether the user wants vendor/check mutation; if not, run `go build` manually.
2. Run the appropriate `just` target.
3. Verify artifact: `ls -lh dist/`.
4. For OCI: confirm image with `<executor> images | grep imapsync-go`.
