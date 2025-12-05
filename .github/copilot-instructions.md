# AI Coding Agent Instructions for imapsync-go

## Project Overview
`imapsync-go` is a lightweight CLI tool for IMAP-to-IMAP folder synchronization. Built with Go 1.25.5, it emphasizes **zero dependencies** (vendored deps), cross-platform support, and efficient parallel message copying between IMAP servers.

## Architecture & Components

### Core Data Flow
1. **CLI Layer** (`cmd/`) - Uses `urfave/cli/v2` for command routing
2. **Config Layer** (`internal/config/`) - Supports JSON/YAML via file extension detection
3. **Client Layer** (`internal/client/`) - Wraps `emersion/go-imap` with auto-reconnect logic
4. **Sync Engine** (`cmd/commands/sync.go`) - Builds sync plans from Message-IDs, streams bodies server-to-server
5. **Progress Display** (`internal/progress/`) - Pre-styled wrapper around `go-pretty/v6/progress`

### Key Architectural Decisions
- **Vendor mode**: All dependencies in `vendor/` - use `-mod vendor` for builds
- **Static binaries**: `CGO_ENABLED=0` + `-extldflags '-static'` for portability
- **Delimiter handling**: Caches IMAP hierarchy delimiter (`.` vs `/`) per server on connect
- **Folder locking**: Global mutex map prevents race conditions during parallel folder creation
- **Message deduplication**: Compares `Message-ID` headers to skip already-synced messages

## Build & Development Workflow

### Task Runner: Just
All common tasks use `Justfile` (not Make):
```bash
just tools     # Install linters (golangci-lint, staticcheck, govulncheck)
just deps      # Tidy and vendor dependencies
just check     # Run vet + staticcheck + govulncheck
just test      # Run tests in internal/
just build     # Build for current OS with version injection with check and vendoring
just build_linux  # Cross-compile for Linux amd64
just oci       # Build container image (uses podman by default)
```

### Version Injection
Builds inject metadata via ldflags:
```go
// cmd/cmd.go
var Version = "dev"   // Set by -X flag from git describe
var Commit = "none"   // Set by git rev-parse --short HEAD
var Date = "unknown"  // Set by date -u
```

### Testing
- Tests live in `internal/*/` packages only (e.g., `config_test.go`, `utils_test.go`)
- Run with `go test ./internal/...` to avoid vendor directory
- Table-driven tests preferred - see `config_test.go` for validation patterns

## Code Conventions

### Package Structure
```
cmd/                    # CLI setup + command routing
  commands/             # Subcommand implementations (show, sync)
internal/
  client/              # IMAP client with reconnect + progress hooks
  config/              # Config loading, validation, type definitions
  progress/            # Pre-styled progress bar wrapper
  utils/               # Small helpers (confirmation prompts, formatters)
```

### Error Handling
- Wrap errors with context: `fmt.Errorf("failed to X: %w", err)`
- Client methods return early on connection errors, caller handles reconnect
- Config validation happens in `validate()` method, called after unmarshal

### IMAP Folder Paths
- **Delimiter mapping**: Transform paths when delimiters differ (e.g., `Archive.2023` → `Archive/2023`)
- Use cached `client.GetDelimiter()` instead of re-querying
- Escape special chars in folder names when constructing selection queries

### Progress Tracking
- Client methods accept optional `ProgressWriter` + `ProgressTracker` interfaces
- Prevents circular deps (progress package imports client types)
- Update trackers in 10-item batches (see `progressUpdateInterval` constant)

## Configuration

### File Format Detection
Config supports `.json`, `.yaml`, `.yml` via `filepath.Ext()` switch in `config.New()`

### Environment Variables
All CLI flags have `IMAPSYNC_*` env var equivalents:
```bash
IMAPSYNC_CONFIG=/path/to/config.json
IMAPSYNC_SOURCE_FOLDER=INBOX
IMAPSYNC_DESTINATION_FOLDER=INBOX
IMAPSYNC_WORKERS=4
IMAPSYNC_VERBOSE=true
```

### Worker Limits
- Default: 4 workers
- Max: 10 workers (enforced in `config.validate()`)
- Set via `-w/--workers` flag

## Common Patterns

### Adding a New Command
1. Create handler in `cmd/commands/<name>.go`
2. Register in `cmd/cmd.go` Commands slice
3. Add flags specific to that command in its definition
4. Use `config.New(cCtx)` to load shared config

### Extending Config Schema
1. Add field to `Config` struct with JSON/YAML tags
2. Update `validate()` method in `config.go`
3. Add test case to `config_test.go` validation table
4. Update `config.example.json` and `config.example.yaml`

### Client Reconnect Logic
- Client wraps methods to auto-retry on connection failures
- Exponential backoff starts at 2s, capped at 10s intervals
- Max 5 reconnect attempts before returning error
- See `client.ensureConnected()` pattern

## External Dependencies
- `emersion/go-imap` - Pure Go IMAP client (no TLS config customization needed for basic use)
- `urfave/cli/v2` - Flag parsing + subcommand routing
- `jedib0t/go-pretty/v6` - Progress bars with Braille characters
- `gopkg.in/yaml.v3` - YAML unmarshaling

## Container Image
- **Base**: `gcr.io/distroless/static-debian13:nonroot` (no shell, minimal attack surface)
- **Entrypoint**: `/imapsync-go` binary only
- Build with `just oci` - copies `dist/imapsync-go` from Linux build
