# imapsync-go

`imapsync-go` is a lightweight Go CLI that mirrors folders between two IMAP accounts. It builds a sync plan from message IDs, streams mail bodies directly between servers, and keeps an encrypted local cache so repeat runs become much faster.

> **Note:** This project was created in collaboration with the GPT 5.1 Codex agentic model.

## todo:

- <https://github.com/jedib0t/go-pretty/blob/main/progress/README.md>

## Features

- IMAP→IMAP copy with per-folder mapping and dry visibility via `show`.
- JSON or YAML configuration with optional environment overrides for every flag.
- Parallel fetching: Source and destination metadata are fetched concurrently.
- Parallel uploads with automatic reconnect/backoff logic and mailbox auto-creation.
- AES-encrypted cache under `~/.imapsync/cache` for mailbox metadata and message IDs.
- Rich CLI experience (spinner, quiet/verbose modes) plus confirmation prompts to avoid accidents.

## Requirements

- Go 1.25+ (module uses the vendor tree for reproducible builds).
- Credentials for both IMAP servers (host:port, user, password, TLS-ready).
- Optional: [`just`](https://github.com/casey/just) for the included automation recipes.

## Installation

### Build directly with Go

```bash
CGO_ENABLED=0 go build -mod vendor -o imapsync-go ./main.go
```

### Using the Justfile targets

```bash
just deps      # go mod tidy && go mod vendor
just lint      # golangci-lint + staticcheck + govulncheck
just check     # vet + staticcheck + govulncheck
just test      # run all tests with verbose output
just test-coverage  # run tests with coverage report
just test-race      # run tests with race detector
just test-html      # generate HTML coverage report
just build     # produces dist/imapsync-go.bin
just build_linux  # cross-compiles Linux/amd64 artifact
```

Binary artifacts embed Git tag/commit metadata via ldflags defined in `Justfile`.

## Configuration

1. Copy one of the samples and edit the values:

   ```bash
   cp config.example.json config.json
   # or: cp config.example.yaml config.yaml
   ```

2. Point the CLI at the file with `--config` or `IMAPSYNC_CONFIG` (defaults to `./config.json`).

3. Run `show` to verify folder visibility before syncing.

### Fields

| Key | Description |
| --- | --- |
| `src`, `dst` | IMAP endpoints. `label` is used in logs/spinner, `server` must include `host:port`. |
| `map` | List of `{"src": "INBOX", "dst": "INBOX"}` entries controlling folder routing. |
| CLI `--src-folder/--dest-folder` | Override mappings for a one-off sync of a single pair. |

### Sample JSON config

```json
{
  "src": {
    "label": "old",
    "server": "mail.oldhost.com:993",
    "user": "user@oldhost.com",
    "pass": "password"
  },
  "dst": {
    "label": "new",
    "server": "imap.newhost.com:993",
    "user": "user@newhost.com",
    "pass": "password"
  },
  "map": [
    { "src": "INBOX", "dst": "INBOX" },
    { "src": "Sent", "dst": "Sent" },
    { "src": "Archive", "dst": "Archive" }
  ]
}
```

YAML carries the same structure; see `config.example.yaml`.

## Usage

### Show the current mailbox layout

```bash
imapsync-go --config config.json show --verbose --cached
```

- Contacts both servers (or loads cache) and prints per-folder counts and disk usage.
- Use `--cached` to prefer the encrypted cache; falls back to live data if stale.

### Synchronize folders

```bash
imapsync-go --config config.json sync --workers 4 --confirm
```

- Builds a sync plan by comparing message IDs per folder.
- Creates destination folders automatically when missing.
- Streams message bodies sequentially or with worker pools (`--workers`).
- Unless `--confirm` (or `-y`) is set, you will be prompted before uploading.

### CLI flags & env vars

| Scope | Flag | Env | Default | Description |
| --- | --- | --- | --- | --- |
| Global | `--config, -c` | `IMAPSYNC_CONFIG` | `config.json` | Path to JSON/YAML config. |
| `show` | `--verbose, -V` | `IMAPSYNC_VERBOSE` | `false` | Print every spinner update. |
| `show` | `--cached` | `IMAPSYNC_CACHED` | `false` | Use cache if available. |
| `sync` | `--src-folder, -s` | `IMAPSYNC_SOURCE_FOLDER` | `''` | Source folder override. |
| `sync` | `--dest-folder, -d` | `IMAPSYNC_DESTINATION_FOLDER` | `''` | Destination folder override. |
| `sync` | `--workers, -w` | `IMAPSYNC_WORKERS` | `4` | Concurrent upload workers (1 disables parallelism). |
| `sync` | `--verbose, -V` | `IMAPSYNC_VERBOSE` | `false` | Chatty logs per message. |
| `sync` | `--quiet, -q` | `IMAPSYNC_QUIET` | `false` | Disable spinner/output. |
| `sync` | `--no-cache` | `IMAPSYNC_NO_CACHE` | `false` | Force live metadata, skip cache saves. |
| `sync` | `--confirm, -y, --yes` | `IMAPSYNC_CONFIRM` | `false` | Skip confirmation prompt. |

## Cache behavior

- Cache files live under `~/.imapsync/cache/<sha256>.cache` and are encrypted with AES-GCM using both accounts’ credentials as the key material.
- `show --cached` reads cached mailbox statistics to avoid touching the servers.
- `sync` automatically updates the destination cache after a successful folder upload unless `--no-cache` is set.
- Delete the cache manually (`rm ~/.imapsync/cache/*.cache`) if servers changed drastically.

## Tips & Troubleshooting

- Run `show` first to ensure logins work and mappings are correct.
- Use fewer workers when dealing with rate-limited servers, or increase when migrating large mailboxes with reliable endpoints.
- The client automatically retries on transient network errors; verbose mode reveals reconnect attempts.
- Add `IMAPSYNC_CONFIG=/path/to/config.yaml` to your shell profile to avoid passing `--config` repeatedly.

## Development

- Format/lint/test via the Justfile targets listed above.
- The code relies on vendored modules; regenerate with `just deps` when dependencies are bumped.
- New features should include README updates plus sample config changes where relevant.

### Running tests

```bash
just test           # Complete suite: unit + race + coverage
```

## LicenseReleased under the MIT License. See `LICENSE` for the full text.
