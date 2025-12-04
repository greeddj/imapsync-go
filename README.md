# imapsync-go

`imapsync-go` is a lightweight Go CLI that mirrors folders between two IMAP accounts. It builds a sync plan from message IDs, streams mail bodies directly between servers.

> **Note:** This project was created in collaboration with the GPT 5.1 Codex agent model.

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
docker pull ghcr.io/greeddj/imapsync-go:latest

# or with podman
podman pull ghcr.io/greeddj/imapsync-go:latest
```

### Binary Release

Download pre-built binaries from [GitHub Releases](https://github.com/greeddj/imapsync-go/releases):

```bash
# Example for Linux amd64
curl -LO https://github.com/greeddj/imapsync-go/releases/latest/download/imapsync-go_<version>_Linux_x86_64.tar.gz
tar xzf imapsync-go_<version>_Linux_x86_64.tar.gz
chmod +x imapsync-go
sudo mv imapsync-go /usr/local/bin/
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
# Show available folders
imapsync-go show -c config.yaml

# Sync all configured folders
imapsync-go sync -c config.yaml -w 4

# Sync specific folder
imapsync-go sync -c config.yaml -s INBOX -d INBOX

# Auto-confirm without prompt
imapsync-go sync -c config.yaml -y
```

### Running with Docker

```bash
# Mount config file and run
docker run --rm -v $(pwd)/config.json:/config.json \
  ghcr.io/greeddj/imapsync-go:latest \
  sync -c /config.json -w 4

# Show folders
docker run --rm -v $(pwd)/config.yaml:/config.yaml \
  ghcr.io/greeddj/imapsync-go:latest \
  show -c /config.yaml

# With podman
podman run --rm -v $(pwd)/config.json:/config.json:Z \
  ghcr.io/greeddj/imapsync-go:latest \
  sync -c /config.json -w 4
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

**Show command:**

- `-V, --verbose` - Enable verbose output

## Examples

```bash
# Sync with 8 workers
imapsync-go sync -c config.yaml -w 8

# Sync specific folder without confirmation
imapsync-go sync -c config.yaml -s INBOX -d INBOX -y

# Verbose output
imapsync-go sync -c config.yaml -V

# Check version
imapsync-go --version
```

## License

MIT
