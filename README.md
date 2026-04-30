# imapsync-go

`imapsync-go` is a lightweight Go CLI that mirrors folders between two IMAP accounts. It builds a sync plan from message IDs, streams mail bodies directly between servers.

> **Note:** This project was created in collaboration with the Claude Code.

## Motivation

This tool was built to provide a **simple, fast, and dependency-free** solution for IMAP synchronization. Key goals:

- **Zero dependencies** - Single static binary with no external requirements
- **Cross-platform** - Works on Linux and macOS (amd64/arm64)
- **Efficient** - Parallel workers for fast synchronization
- **Simple** - Easy configuration via JSON or YAML

Unlike heavyweight alternatives, `imapsync-go` focuses on doing one thing well: efficiently copying IMAP folders between servers.

## Installation

### Homebrew (macOS)

```bash
brew tap greeddj/tap
brew install imapsync-go
```

### Docker

Pull the image from GitHub Container Registry:

```bash
# Latest version
podman pull ghcr.io/greeddj/imapsync-go:latest
# or
podman pull ghcr.io/greeddj/imapsync-go:1.0.0
```

### Binary Release

Download pre-built binaries from [GitHub Releases](https://github.com/greeddj/imapsync-go/releases):

```bash
# Example for Linux amd64
curl -LO https://github.com/greeddj/imapsync-go/releases/latest/download/imapsync-go_<version>_Linux_x86_64.tar.gz
tar xzf imapsync-go_<version>_Linux_x86_64.tar.gz
chmod +x imapsync-go
sudo mv imapsync-go /usr/local/bin/
sudo xattr -rd com.apple.quarantine /usr/local/bin/imapsync-go
```

## Usage

### Configuration

Create a configuration file (`config.json` or `config.yaml`):

**JSON example:**

```json
{
  "src": {
    "label": "Source",
    "server": "imap.source.com:993",
    "user": "user@source.com",
    "pass": "password"
  },
  "dst": {
    "label": "Destination",
    "server": "imap.dest.com:993",
    "user": "user@dest.com",
    "pass": "password"
  },
  "map": [
    {"src": "INBOX", "dst": "INBOX"},
    {"src": "Sent", "dst": "Sent Items"}
  ]
}
```

**YAML example:**

```yaml
src:
  label: Source
  server: imap.source.com:993
  user: user@source.com
  pass: password

dst:
  label: Destination
  server: imap.dest.com:993
  user: user@dest.com
  pass: password

map:
  - src: INBOX
    dst: INBOX
  - src: Sent
    dst: Sent Items
```

### Running with Homebrew

```bash
export IMAPSYNC_CONFIG="/Users/$(whoami)/.imapsync/prod_config.json"

# Show available folders
imapsync-go show

# Sync all configured folders
imapsync-go sync -w 4

# Sync specific folder
imapsync-go sync -s INBOX -d INBOX
imapsync-go sync -s 'Test.[some_group].box' -d 'Test/some_group/box'

# Auto-confirm without prompt
imapsync-go sync -y
```

### Running with Docker

```bash

# show folders
podman run --rm \
-v /Users/$(whoami)/.imapsync/prod_config.json:/config.json ghcr.io/greeddj/imapsync-go:latest \
-c /config.json \
show

# sync folders
podman run --rm -it \
-v /Users/$(whoami)/.imapsync/prod_config.json:/config.json ghcr.io/greeddj/imapsync-go:latest \
-c /config.json \
sync -w 4
```

### Command-line Options

**Global flags:**

- `-c, --config` - Path to configuration file (default: `config.json`)

**Sync command:**

- `-s, --src-folder` - Source folder (overrides config)
- `-d, --dest-folder` - Destination folder (overrides config)
- `-w, --workers` - Number of parallel workers (default: 4, max: 10)
- `-y, --confirm` - Auto-confirm without prompt
- `-V, --verbose` - Enable verbose output
- `-q, --quiet` - Suppress non-error output
- `--bps-down` - Max bytes/sec read from the source server (0 = unlimited)
- `--bps-up` - Max bytes/sec written to the destination server (0 = unlimited)
- `--max-connections` - Hard cap on simultaneous IMAP connections per side (0 = workers)

The same values can be set in config under a `rate_limit` block (`down_bps`,
`up_bps`, `max_connections`) and via env vars `IMAPSYNC_BPS_DOWN`,
`IMAPSYNC_BPS_UP`, `IMAPSYNC_MAX_CONNECTIONS`. Flags win over config when
both are set.

## Provider quotas (Gmail)

When either side is `imap.gmail.com`, `imapsync-go` prints a warning before
the confirm prompt with the relevant Workspace IMAP limits:

- 15 simultaneous IMAP connections per account
- 2,500 MB/day download, 500 MB/day upload via IMAP
- Exceeding the bandwidth limit suspends the account for 1–24 hours

For Gmail, run with explicit throttling so the daily quotas don't trigger an
account lockout mid-migration:

```bash
imapsync-go sync \
  --bps-down 300000 \
  --bps-up 300000 \
  --max-connections 10 \
  -w 4
```

The defaults (no throttle) match `imapsync`'s default behaviour — they're
appropriate for self-hosted IMAP servers on a LAN, not for big-provider
mailboxes.

## Idempotency caveats

`imapsync-go` decides what to copy by diffing message **`Message-Id`** headers
between source and destination. This is fast and re-running a partial sync is
safe, but it has two known limitations:

- **Messages without a `Message-Id` are skipped.** Drafts, some bulk mail and
  messages from broken senders may not have one. They cannot be tracked across
  servers, so they are never copied. The CLI prints a warning per folder when
  this happens so you know how many were skipped.
- **Servers that rewrite `Message-Id` on `APPEND` will cause duplicates on
  re-run.** A few IMAP servers (notably some Exchange configurations) replace
  the inbound `Message-Id` with their own value. The diff on the next run will
  see the original IDs as still "missing" on the destination and re-upload
  them. If you suspect this, do the sync in a single pass and avoid re-running
  it against the same destination.

## License

MIT
