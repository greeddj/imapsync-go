# imapsync-go

[![CI](https://github.com/greeddj/imapsync-go/actions/workflows/ci.yml/badge.svg)](https://github.com/greeddj/imapsync-go/actions/workflows/ci.yml)
[![Release](https://github.com/greeddj/imapsync-go/actions/workflows/release.yml/badge.svg)](https://github.com/greeddj/imapsync-go/actions/workflows/release.yml)
[![codecov](https://codecov.io/gh/greeddj/imapsync-go/graph/badge.svg)](https://codecov.io/gh/greeddj/imapsync-go)

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

Requires Homebrew 6.0.13 or newer: the cask uses the `postflight_steps` install-step
DSL, and an older Homebrew fails to load it rather than failing to install it.

### Docker

Pull the image from GitHub Container Registry:

```bash
# Latest stable release
podman pull ghcr.io/greeddj/imapsync-go:latest
# or a specific version
podman pull ghcr.io/greeddj/imapsync-go:1.2.0
```

`:latest` moves only on stable releases. A prerelease is published under its
version tag alone. `:<version>` and `:latest` are multi-architecture manifests;
per-architecture tags such as `:1.2.0-amd64` and `:latest-arm64` are no longer
published.

### Binary Release

Download pre-built binaries from [GitHub Releases](https://github.com/greeddj/imapsync-go/releases):

```bash
# Example for Linux amd64
curl -LO https://github.com/greeddj/imapsync-go/releases/latest/download/imapsync-go_<version>_Linux_x86_64.tar.gz
tar xzf imapsync-go_<version>_Linux_x86_64.tar.gz
chmod +x dist/imapsync-go
sudo mv dist/imapsync-go /usr/local/bin/
```

The archive carries the binary under `dist/`, beside `LICENSE`, `README.md` and
the two example configs.

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
    "pass": "password",
    "auth": "login"
  },
  "dst": {
    "label": "Destination",
    "server": "imap.dest.com:993",
    "user": "user@dest.com",
    "pass": "password",
    "auth": "cram-md5"
  },
  "map": [
    { "src": "INBOX", "dst": "INBOX" },
    { "src": "Sent", "dst": "Sent Items" }
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
  auth: login

dst:
  label: Destination
  server: imap.dest.com:993
  user: user@dest.com
  pass: password
  auth: cram-md5

map:
  - src: INBOX
    dst: INBOX
  - src: Sent
    dst: Sent Items
```

> **Note:** The `map` section is optional. If omitted, `imapsync-go`
> automatically connects to the source server, lists all mailboxes, and
> builds a 1:1 mapping of every source folder to the same-named destination
> folder. You can also use `--src-folder` / `--dest-folder` to sync a single
> folder without a config file `map` section.

The command checks the configuration file when it loads the file.

- The file can contain only these keys: `src` and `dst` (`label`, `server`,
  `user`, `pass`, `auth`), `map` (items with `src` and `dst`), and `rate_limit`
  (`down_bps`, `up_bps`, `max_connections`).
- If the file contains a different key at any level, the command stops with an
  error. The error shows the key and the path of the file.
- If a `map` item has no `src` or no `dst`, or one of them is empty, the
  command stops with an error. The error shows the number of the item, counted
  from 1, and the key.
- In a JSON file, the letter case of a key is not important. For example,
  `SRC` and `src` are the same key. In a YAML file, a key must have the same
  letter case as in this list. For example, `Src` causes an error.
- A JSON file must contain one object. A YAML file must contain one document with the settings. After this document, the file can contain only empty documents or null documents, for example `---` or `~`.
- If `map` has no items, the command does not stop with an error. The command
  copies all source folders, the same as for a file without `map`.
- These values have no items: `"map": []`, `"map": null`, `map: []`, and a
  `map:` key with all items in comments.

> **Caution:** Make sure that `map` has items before you use `-y`. With `-y`,
> the command does not ask you before it starts to copy.

### Authentication

The `auth` field in `src` or `dst` blocks is optional and specifies the authentication mechanism:

- `login` (default): Standard IMAP LOGIN authentication.
- `cram-md5`: CRAM-MD5 Challenge-Response authentication.

If omitted or set to an empty string, `login` is used.

The letter case of the value is not important. `CRAM-MD5` and `cram-md5` select the same mechanism.

If the value is any other text, the command stops before it connects to a server. The error shows the block (`src` or `dst`) and the supported values. The error does not show the value from the file.

### Running with Homebrew

```bash
export IMAPSYNC_CONFIG="/Users/$(whoami)/.imapsync/prod_config.json"

# Show available folders
imapsync-go show

# Sync all folders (from config map or auto-discovered from source)
imapsync-go sync -w 4

# Sync a single folder
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

**Show command:**

- `-V, --verbose` - Show additional detail (env: `IMAPSYNC_VERBOSE`)
- `-q, --quiet` - Suppress progress bars; output is plain text suitable for piping (env: `IMAPSYNC_QUIET`)

**Sync command:**

- `-s, --src-folder` - Source folder (overrides config) (env: `IMAPSYNC_SOURCE_FOLDER`)
- `-d, --dest-folder` - Destination folder (overrides config) (env: `IMAPSYNC_DESTINATION_FOLDER`)
- `-w, --workers` - Number of parallel workers (default: 4, max: 10) (env: `IMAPSYNC_WORKERS`)
- `-y, --confirm, --yes` - Auto-confirm without prompt. The `-q` option needs this option. (env: `IMAPSYNC_CONFIRM`)
- `-V, --verbose` - Enable verbose output (env: `IMAPSYNC_VERBOSE`)
- `-q, --quiet` - Hide the progress bars, the sync plan and the status messages. Returned errors, the delimiter notice and the list `Folders that cannot be synced` still show. The final line also shows when the sync copies messages or has a failed folder. The reason for a failed message does not show. This option needs `-y`. Without `-y`, the sync stops with an error before it connects to a server. (env: `IMAPSYNC_QUIET`)
- `--bps-down` - Max bytes/sec read from the source server (0 = unlimited) (env: `IMAPSYNC_BPS_DOWN`)
- `--bps-up` - Max bytes/sec written to the destination server (0 = unlimited) (env: `IMAPSYNC_BPS_UP`). The limit is shared by all workers. Below about 110 bytes/sec for each worker, an upload can wait so long that the 5-minute idle limit closes the connection, so `sync` prints a warning before the plan.
- `--max-connections` - Hard cap on simultaneous IMAP connections per side (0 = use the `--workers` value). One slot is reserved for the planning client, so `--max-connections=N` allows at most N−1 sync workers. (env: `IMAPSYNC_MAX_CONNECTIONS`)

The same `bps-down`, `bps-up`, and `max-connections` values can be set in config under a `rate_limit` block (`down_bps`, `up_bps`, `max_connections`). CLI flags take precedence when both are set. This is also true when the value is 0. For example, `--bps-down 0` or `IMAPSYNC_BPS_DOWN=0` removes the `down_bps` limit of the config file.

## Folders that `sync` cannot copy or `show` cannot read

Some servers list a folder that cannot hold messages. For example, Gmail lists `[Gmail]`. The server marks such a folder `\Noselect`.

- `sync` does not scan a source folder that the server marks `\Noselect`. It copies the subfolders of this folder.
- With `--verbose` and without `-q`, `sync` shows one line for each such folder.
- If a destination folder is `\Noselect` and the source folder has at least one message with a `Message-Id`, `sync` cannot copy the messages. `sync` adds the mapping to the list `Folders that cannot be synced`.
- `show` shows `container` in the Messages column for such a folder.

### Failed folders in `sync`

If `sync` cannot scan a source folder, it does not stop. It copies the other folders.

- The command prints the list `Folders that cannot be synced`. Each line shows the source folder, the destination folder and the reason.
- The `-q` option does not hide the list.
- After the copy, the final line shows the number of failed folders, and the command ends with exit code 1.
- If no folder has new messages and a folder failed, the command asks no question. It prints the list and ends with exit code 1.
- If you answer no to the question before the copy, the command copies no messages and ends with exit code 0. This is also true when a folder failed.

Some servers list the subfolders of a folder, but not the folder. If such a folder refuses to open, `sync` skips it and copies the subfolders. The command prints one line for the folder, and the `-q` option hides this line. The exit code does not change. `sync` never skips INBOX in this way.

### Unreadable folders in `show`

If `show` cannot read a folder, it shows `unreadable` in the Messages column and `-` in the Size column. The message total and the size total do not include this folder. The folder total includes it. The footer also shows the number of unreadable folders.

After the tables, the command prints the list `Folders that cannot be read`. Each line shows the account (`Source` or `Destination`), the folder and the reason. The `-q` option does not hide the list. The command ends with exit code 1.

## Provider quotas (Gmail)

When either side is `imap.gmail.com`, `imapsync-go` prints a warning before
the confirm prompt with the relevant Workspace IMAP limits:

- 15 simultaneous IMAP connections per account
- 2,500 MB/day download, 500 MB/day upload via IMAP
- Exceeding the bandwidth limit suspends the account for 1-24 hours

For Gmail, run with explicit throttling so the daily quotas don't trigger an
account lockout mid-migration:

```bash
imapsync-go sync \
  --bps-down 300000 \
  --bps-up 300000 \
  --max-connections 10 \
  -w 9
```

`--max-connections 10` reserves one slot for the planning client, leaving 9
slots for workers. `-w 9` matches that ceiling; setting `-w` higher than
`maxConn−1` is harmless - the cap is enforced automatically.

The defaults (no throttle) match `imapsync`'s default behaviour - they're
appropriate for self-hosted IMAP servers on a LAN, not for big-provider
mailboxes.

## Notes

- **Exit code 1** - `sync` ends with code 1 when a message or a folder fails. If you answer no to the question before the copy, `sync` ends with code 0, also when a folder failed. `show` ends with code 1 when it cannot read a folder. Any other error, except a cancel with Ctrl-C, prints `Error:` and ends with code 1.
- **Ctrl-C** exits with code 130 and prints `Cancelled.` - this is the standard Unix convention for SIGINT termination and makes it composable in shell scripts.
- **Ctrl+\\** (SIGQUIT) writes the stacks of all goroutines to stderr and exits with code 131.
- **`--version` on source builds** makes one HTTPS request to the GitHub releases API to fetch the latest tag. Set `IMAPSYNC_OFFLINE=1` or run inside a CI environment (any non-empty `CI` variable) to skip that call and fall back to the build-time default. Other commands and `--help` make no request.

## Terminal output

A server can send folder names, error messages and Message-Ids that contain control or invisible characters. These characters can move the cursor, remove text from the screen or hide text.

`imapsync-go` shows these characters as escape text. For example, a folder name with an ESC character, a carriage return and a right-to-left override shows as `Evil\x1b[2J\rx\u202egpj`.

- The escape forms are `\a`, `\b`, `\f`, `\n`, `\r`, `\t`, `\v`, `\xNN`, `\uNNNN` and `\UNNNNNNNN`.
- Letters of all scripts, punctuation, symbols, emoji and spaces do not change.
- Some status messages show invisible letters, for example `\u3164`, without change. These messages are the delimiter messages, the folder-creation messages and the skipped-mapping message. Control characters in these messages still show as escape text.
- Only the display changes. The program sends the exact folder names to the servers.
- The final `Error:` line escapes line breaks too. A multi-line error, for example a YAML type error in the config file, prints on one line with `\n` between the parts.
- The line breaks are escaped on purpose. A raw line break lets a server make a false status line after `Error:`.

The escaping is for UTF-8 terminals.

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

## Known issues

### Ctrl+C may not stop a stalled upload

To upload a message, the program first sends the size of the message. Then it
waits for the server to accept the upload. If the server or the network stops
in this step, Ctrl+C does not stop the program. The idle limit and TCP
keepalive close the connection, but the program continues to wait.

In this case, press Ctrl+\ (SIGQUIT). The program writes the stacks of all
goroutines to stderr and exits with code 131.

At all other steps, Ctrl+C stops the program at once.

### Limits during connection, commands and exit

These limits apply to the connection steps, to the commands and to the exit of the program:

- During the greeting, the login and the mailbox list, Ctrl+C stops the program at once.
- If a server accepts a connection but sends no greeting, the connection attempt fails after 30 seconds.
- If no data moves for 5 minutes during the login, the connection attempt fails.
- During a command, the program watches for data in both directions. If no data moves for 5 minutes, the program closes the connection and connects again. Then it sends the command one more time.
- If the program closes the connection during the upload of a message, it does not send that message again. The next run copies it.
- A transfer that moves data has no time limit. The program does not close a connection that is idle between commands.
- When the program exits, it logs out all connections at the same time. A `sync` waits a maximum of 4 seconds for this, and a `show` waits a maximum of 2 seconds.

The 5-minute limit does not change the Ctrl+C entry above.

## License

MIT
