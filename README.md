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
> builds a 1:1 mapping of every source folder to the destination folder with
> the same name. If the two servers use different hierarchy delimiters, the
> command converts the name. Refer to
> [Folder names and hierarchy delimiters](#folder-names-and-hierarchy-delimiters).
> The command creates a destination folder only when the sync plan copies a message to this folder or to one of its subfolders.
> Refer to
> [Destination folders that the command creates](#destination-folders-that-the-command-creates).
> You can also use the options `-s` and `-d`. If you use them, the command does
> not use the `map` section. Refer to [Command-line Options](#command-line-options).

A `map` item applies to its source folder and to all subfolders of this folder.
The options `-s` and `-d` give one item of this type. For example, on two servers
that use `/`, `-s Projects -d Archive` copies `Projects` to `Archive` and
`Projects/2024` to `Archive/2024`.
If two `map` items apply to one folder, the item with the nearest source folder
decides. The order of the items does not change the result. For example, with
`INBOX` to `Old` and `INBOX/Work` to `Work`, the command copies `INBOX/Work` to
`Work`.

The command checks the configuration file when it loads the file.

- The file can contain only these keys: `src` and `dst` (`label`, `server`,
  `user`, `pass`, `auth`), `map` (items with `src` and `dst`), and `rate_limit`
  (`down_bps`, `up_bps`, `max_connections`).
- If the file contains a different key at any level, the command stops with an
  error. The error shows the key and the path of the file.
- If a `map` item has no `src` or no `dst`, or one of them is empty, the
  command stops with an error. The error shows the number of the item, counted
  from 1, and the key.
- If two `map` items have the same `src` and different `dst` values, the
  command stops with an error. The error shows the numbers of the two items,
  counted from 1.
- Two identical items are valid. Two items with the same `dst` are also valid.
  `sync` uses identical items as one item.
- `sync` makes this check again after it finds the folder names on the server.
  For example, `inbox` and `INBOX` are one source folder.
- `sync` also refuses a `dst` name that has an empty level and that the
  destination server does not list. Refer to
  [Names that you type](#names-that-you-type).
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

# Sync one folder and all its subfolders
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
- `-q, --quiet` - Hide the progress bars. The output holds no colour codes, so a pipe can read it (env: `IMAPSYNC_QUIET`)

**Sync command:**

- `-s, --src-folder` - Source folder. The command copies this folder and all its subfolders. You must also use `-d`. If you use `-s` and `-d`, the command does not use the `map` items of the config file. (env: `IMAPSYNC_SOURCE_FOLDER`)
- `-d, --dest-folder` - Destination folder for `-s`. The subfolders of the source folder go below this folder. The command refuses a name with an empty level that the destination server does not list (refer to [Names that you type](#names-that-you-type)). (env: `IMAPSYNC_DESTINATION_FOLDER`)
- `-w, --workers` - Number of parallel workers (default: 4, max: 10) (env: `IMAPSYNC_WORKERS`)
- `-y, --confirm, --yes` - Auto-confirm without prompt. The `-q` option needs this option. (env: `IMAPSYNC_CONFIRM`)
- `-V, --verbose` - Enable verbose output (env: `IMAPSYNC_VERBOSE`)
- `-q, --quiet` - Hide the progress bars, the sync plan and the status messages. Returned errors, the delimiter notice and the list `Folders that cannot be synced` still show. The notices of a renamed folder, of a left-out folder and of destination folders that are different only in letter case also still show. The final line also shows when the sync copies messages or has a failed folder. After a stop, the `Sync stopped` line and the final line also show. The reason for a failed message does not show. This option needs `-y`. Without `-y`, the sync stops with an error before it connects to a server. (env: `IMAPSYNC_QUIET`)
- `--bps-down` - Max bytes/sec read from the source server (0 = unlimited) (env: `IMAPSYNC_BPS_DOWN`)
- `--bps-up` - Max bytes/sec written to the destination server (0 = unlimited) (env: `IMAPSYNC_BPS_UP`). The limit is shared by all workers. Below about 110 bytes/sec for each worker, an upload can wait so long that the 5-minute idle limit closes the connection, so `sync` prints a warning before the plan.
- `--max-connections` - Hard cap on simultaneous IMAP connections per side (0 = use the `--workers` value). The planning session closes before the workers connect, so `--max-connections=N` gives at most N sync workers. (env: `IMAPSYNC_MAX_CONNECTIONS`)

The same `bps-down`, `bps-up`, and `max-connections` values can be set in config under a `rate_limit` block (`down_bps`, `up_bps`, `max_connections`). CLI flags take precedence when both are set. This is also true when the value is 0. For example, `--bps-down 0` or `IMAPSYNC_BPS_DOWN=0` removes the `down_bps` limit of the config file.

## Folder names and hierarchy delimiters

An IMAP server uses a hierarchy delimiter to split a folder name into levels. For example, `Archive/2023` has two levels when the delimiter is `/`. Some servers use `.`. A flat server has no delimiter.

### Names that you type

You can type a folder name in `map`, or with `-s` and `-d`. The command changes a delimiter in a typed name only when all these conditions are true:

- The server of that side does not list the name.
- The name holds `/`, `.` or `\`, and this character is not the delimiter of the server.
- The server lists the name when the delimiter of the server replaces this character.

The command shows each change in the notice `Folder path delimiter mismatch detected` and asks one question. With `-y`, the command does not ask. If you answer no, the command keeps the names as you typed them.

The command never changes a name that the server lists. For example, a folder `example.com` on a `/` server keeps its name.

If the server does not list a typed source name, and the server lists no changed name, the command keeps the typed name. The command sends this name to the server. If the server refuses it, the folder appears in the list `Folders that cannot be synced`.

If the server does not list a typed destination name, and the server lists no changed name, the command keeps the typed name. It creates the folder with this name, unless the name has an empty level. For example, if a `.` server lists neither `Archive/2024` nor `Archive.2024`, the command sends CREATE for `Archive/2024`.

The command refuses a typed destination name when all these conditions are true:

- The destination server has a delimiter. A flat server has no levels, so the command never refuses a name there.
- The name has an empty level. This means that the name starts with the delimiter, ends with the delimiter, or holds the delimiter two times in a row. For example, `/X`, `x/` and `a//b` have an empty level on a `/` server.
- The destination server does not list the name. The command does not refuse a listed name, because it creates no folder for it.

If the command refuses a name, it stops with an error before it scans the folders. The error shows the name and the delimiter. It also shows the number of the `map` item, counted from 1, or the option `--dest-folder`:

- `map entry 2: destination folder has an empty level: "a//b", destination delimiter "/"`
- `--dest-folder: destination folder has an empty level: "x/", destination delimiter "/"`

The command sends no EXAMINE, STATUS, FETCH, CREATE or APPEND to a server. It shows no plan. If more than one `map` item has such a name, the error shows the first of these items. This rule is for the names that you type. A destination name that the command builds can also have an empty level. Refer to [Names that the command converts](#names-that-the-command-converts).

### Names that the command converts

If `map` has no items and you do not use `-s` and `-d`, the command builds the destination name from the source name. It does the same for each subfolder of a mapped folder. It replaces the delimiter of the source server with the delimiter of the destination server. The folder that you give with `-s` is also a mapped folder.

If a level of the source name holds the delimiter of the destination server as text, the command replaces this character with `_`. It uses `-` when the delimiter of the destination server is `_`. For example, `Clients/acme.com` on a `/` server becomes `Clients.acme_com` on a `.` server.

- The command prints one warning line for each renamed folder before the plan. The line shows also without `--verbose` and with `-q`.
- If a renamed folder gets the same destination name as another folder, the command copies none of these folders. Each of these folders appears in the list `Folders that cannot be synced`.

The command never converts a destination name that you type.

A built destination name can have an empty level. For example, if both servers use `/`, a source folder `x/` or `a//b` gives such a name. The command does not copy this folder when all these conditions are true:

- The destination server has a delimiter. A flat server has no levels, so the command never refuses a name there.
- The name starts with the delimiter, ends with the delimiter, or holds the delimiter two times in a row.
- The destination server does not list the name. The command does not refuse a listed name, because it creates no folder for it.

A server can read the CREATE of `x/` as the CREATE of `x`. Then the upload goes to a folder that the command did not create. So the folder fails before the scan. The command copies the other folders.

- The folder appears in the list `Folders that cannot be synced`. The reason is `destination folder has an empty level: "NAME", destination delimiter "D"`.
- The command sends no EXAMINE, STATUS, CREATE or APPEND for this folder.
- The `-q` option does not hide the list. Refer to [Failed folders in `sync`](#failed-folders-in-sync).

### Flat servers

If the server answers NIL for the delimiter of each folder, or lists no folder, the command treats the server as flat. A flat server has no levels.

- The command creates no parent folder on a flat destination.
- A flat source has no subfolders. Each mapped folder gives one folder.
- On a flat destination, a subfolder keeps the delimiter of the source as part of its name.

### Destination folders that the command creates

`sync` creates a destination folder only for a plan that copies at least one message. It also creates the missing parent folders of this folder. Refer to [Parent folders that the command creates](#parent-folders-that-the-command-creates).

A source folder gives no plan when it has no message to copy. Then the command sends no CREATE for it. These are examples:

- The source folder is empty.
- No message in the source folder has a `Message-Id`. Refer to [Idempotency caveats](#idempotency-caveats).
- The destination server lists the destination folder, and this folder already holds each message of the source folder.

So an empty source folder gets no destination folder. `sync` copies messages. It does not copy empty folders. This rule applies to `map` items and to a run without `map` items.

An empty source folder can be the parent of a folder that has messages to copy. Then the command creates its destination folder as a parent folder.

Another source folder can have the same destination folder and messages to copy. Then the command creates this destination folder for that source folder.

If no source folder has a message to copy and no folder failed, the command creates no folder and asks no question. Without `-q`, it prints `All folders already synced!`.

A later run creates the destination folder when the source folder gets a message to copy.

### Parent folders that the command creates

If a destination folder has a parent that the destination server does not list, the command creates the parent before the folder.

- The plan shows these parents in the list `Folders to be created on destination`. Each folder shows one time.
- The list, the counter `Creating folders` and the CREATE commands use one order. The command sorts the destination folders by the bytes of their names, so an upper-case letter comes before a lower-case letter. For each folder, it lists the missing parents from the top, and then the folder. For example, `Old/Z` and `New/A` give `New`, `New/A`, `Old` and `Old/Z`, also when `map` lists `Old/Z` first.
- The list shows a maximum of 20 names. If there are more than 20 folders, the list then shows the line `• … and N more`. The command sends a CREATE for each folder.
- A parent that the server marks `\Noselect` counts as listed. The command does not create it.
- On a destination server with a delimiter, the command sends no CREATE for a name that has an empty level. Refer to [Names that you type](#names-that-you-type) and [Names that the command converts](#names-that-the-command-converts).
- The counter `Creating folders` counts the parents and the folders. At the end, it shows `Created N folders`. If the server refuses a folder, it shows `Created N of M folders, R refused`.

### INBOX

The letter case of a leading `INBOX` level is not important. `inbox`, `Inbox` and `INBOX` select the same folder. On a `/` server, `inbox/Work` and `INBOX/Work` select the same subfolder when the server lists only one of these spellings. If the server lists two spellings, for example `INBOX/Work` and `Inbox/Work`, each spelling is a different folder. The command sends the spelling that the server lists.

### Names that are not valid modified UTF-7

A server sends folder names in modified UTF-7. Some names are not valid. For example, a name can be raw UTF-8, or it can hold an `&` that has no encoding. The command leaves such a folder out and copies the other folders.

- The command prints one line for each left-out folder: `[label] Folder NAME is left out: the server sent a name that is not valid modified UTF-7`.
- The line shows also with `-q`. The `show` command prints it too.
- The exit code does not change.

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
- A destination folder that the command reads again after CREATE can fail in the same way. This happens when the command cannot scan this folder or its source folder again. It also happens when the server changed the UIDVALIDITY of the source folder or did not send it. The command prints the list at once. Refer to [Idempotency caveats](#idempotency-caveats).
- If the command builds a destination name with an empty level and the destination server does not list it, the folder fails before the scan. The reason is `destination folder has an empty level`. Refer to [Names that the command converts](#names-that-the-command-converts).
- If the destination server refuses CREATE for a folder, `sync` does not stop. The list shows each mapping that needs this folder, with the reply of the server. The command copies the other folders. If the server refuses a parent folder, each folder below it fails. A reply that reports a rate limit stops `sync`. Refer to [Provider quotas (Gmail)](#provider-quotas-gmail).

Some servers list the subfolders of a folder, but not the folder. If such a folder refuses to open, `sync` skips it and copies the subfolders. The command prints one line for the folder, and the `-q` option hides this line. The exit code does not change. `sync` never skips INBOX in this way.

### Unreadable folders in `show`

If `show` cannot read a folder, it shows `unreadable` in the Messages column and `-` in the Size column. The message total and the size total do not include this folder. The folder total includes it. The footer also shows the number of unreadable folders.

After the tables, the command prints the list `Folders that cannot be read`. Each line shows the account (`Source` or `Destination`), the folder and the reason. The `-q` option does not hide the list. The command ends with exit code 1.

## How `sync` counts messages

Each message in the plan ends in one of three ways.

- **Copied.** The destination server accepted the upload.
- **Error.** The upload failed, or the source did not send the message. The final line counts it as an error, and the command ends with exit code 1.
- **Gone.** The source folder no longer holds the message. For example, someone deleted it after the plan. This is not an error, and the exit code does not change. Without `-q`, the command prints one line. The line is `Source folder ... no longer holds N of the M planned messages for ..., so they are not copied`.

A source server can refuse to send some messages. For example, one fetch of up to 500 messages can fail.

- The command does not stop for one failed fetch. It goes on with the next 500 messages.
- The command counts each message that the source still holds and that it did not copy as one error. The next run tries these messages again.
- For each folder, the command prints one line `Stream error for folder ...`. If more than one fetch failed, the line ends with `(N batches failed)`. The `-q` option hides this line.
- The command stops the fetch of a folder at once if the source refuses to open the folder or if the UIDVALIDITY changes. It counts each message that it did not copy as an error.
- A response without a message body counts as a message that the source did not send.

## Flags and dates of copied messages

`sync` copies the flags and the internal date of a message with the upload.

- The command copies the keywords and the flags `\Seen`, `\Answered`, `\Flagged` and `\Draft`.
- The command does not copy `\Recent`, because an upload cannot set it. It does not copy `\Deleted`, because an expunge of the folder removes the copy.
- The command drops each other system flag, because a server can refuse an unknown system flag. It also drops a flag that is not valid.
- If no flag stays, the upload has no flags. The copy is then unread on the destination.
- The internal date of the copy is the internal date of the source message. If the source server sends no internal date, the command uses the `Date` header of the message. If the message has no `Date` header, the destination server sets the date.
- The command copies the flags and the date one time, when it uploads the message. If you change a flag on the source later, the next run does not change the copy.

## Provider quotas (Gmail)

When the source or the destination is `imap.gmail.com`, `sync` shows a warning before the question. The `-q` option hides the warning. The warning shows these limits:

- 15 IMAP sessions at the same time for each account.
- 2,500 MB of download and 500 MB of upload for each account each day.

If an account goes above a limit, Gmail can stop its IMAP access for 1 to 24 hours.

For each Gmail side, the warning also names the daily quota of the direction that `sync` uses. This is the upload quota for a destination and the download quota for a source.

A rate limit does not decrease the total quantity of data. It only spreads the transfer over a longer time. The daily quota limits the total for each day.

A higher rate gets to the quota sooner. For example, at 300000 bytes per second the upload gets to 500 MB in approximately 28 minutes. Then the server refuses the transfer, and `sync` stops with the `Sync stopped` line. A later run continues where this run stopped.

These rates keep a non-stop run below the daily quotas, with a margin of at least 5%:

```bash
imapsync-go sync \
  --bps-down 27000 \
  --bps-up 5000 \
  --max-connections 10 \
  -w 10
```

- At 27000 bytes per second, one day moves a maximum of approximately 2,333 MB from the source.
- At 5000 bytes per second, one day moves a maximum of approximately 432 MB to the destination.
- A mailbox that is larger than the daily quota needs more than one day. For example, 3 GB at 5000 bytes per second needs approximately 7 days.
- All workers share one limit, so more workers do not move more data in one day.

`--max-connections 10` limits each side to 10 sessions at the same time. The
planning session closes before the workers connect, so all 10 sessions are for
workers. `-w 10` uses all of them. A `-w` value above `--max-connections` is
safe, because the command applies the cap automatically.

By default, `sync` has no rate limit, as `imapsync` has. This is correct for a self-hosted server on a LAN. It is not correct for a large provider.

When a server reports a rate limit or a full quota, `sync` stops. This can happen in three steps. These steps are the creation of the destination folders, the second read of a destination folder or its source folder after CREATE, and the copy. The command prints this line one time, also with `-q`:

```text
⛔ Sync stopped: the destination server [dst] reports a rate limit or a full quota: <reply>. Run sync again later: it continues where this run stopped.
```

The line names the side (`source` or `destination`), the label of the server and the reply of the server. `sync` counts each message that it did not copy as an error. It ends with exit code 1. It does not wait and it does not print `Cancelled.` Run `sync` again later. The next run continues where this run stopped.

Some servers refuse an upload before they ask for the message data. These servers hide the reason. `sync` then counts one failed message and continues.

A refusal that repeats a folder name can stop `sync`. Some servers repeat the folder name in a refusal, and a folder name can hold a phrase such as `Quota exceeded`. If a person or a program deletes or renames such a folder during the copy, `sync` can stop with the `Sync stopped` line. The reply in the line shows the cause. The run loses no mail. The next run makes a new plan from the current folders.

## Notes

- **Exit code 1** - `sync` ends with code 1 when a message or a folder fails. If you answer no to the question before the copy, `sync` ends with code 0, also when a folder failed. `show` ends with code 1 when it cannot read a folder. An unknown command prints `Error: unknown command` with the name of the command and ends with code 1. Any other error, except a cancel with Ctrl-C or SIGTERM, prints `Error:` and ends with code 1. The commands `imapsync-go help` and `imapsync-go help COMMAND` show the help and end with code 0.
- **Memory** - Each worker holds at most two messages in memory at one time. These are the message that it uploads and the message that it reads next. So the memory use grows with the number of workers and with the size of the largest messages.
- **Progress rows** - During the copy, the screen shows one progress row for each worker that copies a plan. A finished plan keeps its last line on screen. The final summary line also stays on screen.
- **Ctrl-C** exits with code 130 and prints `Cancelled.` - this is the standard Unix convention for SIGINT termination and makes it composable in shell scripts.
- **SIGTERM** (for example from `kill` or `podman stop`) prints `Cancelled.` and exits with code 143, which is 128 plus the signal number.
- **Ctrl+\\** (SIGQUIT) writes the stacks of all goroutines to stderr and exits with code 131.
- **`--version` on a build without a version** (for example `go build` with no `-X main.Version`) makes one HTTPS request to the GitHub releases API. The request fetches the latest tag. Set `IMAPSYNC_OFFLINE=1` or run inside a CI environment (any non-empty `CI` variable) to skip that call and fall back to the build-time default. Other commands and `--help` make no request. The recipes `just build` and `just build_linux` set the version from `git describe`, so these binaries make no request.

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

`imapsync-go` decides what to copy from the **`Message-Id`** headers of the
messages. A copy is one message with a given `Message-Id`. For each
`Message-Id`, the command counts the copies in the source folder and the copies
in the destination folder. If the source folder holds more copies, the command
copies the difference. The command never deletes a copy on the destination.
This is fast, and a new run after a partial sync is safe.

A destination server that advertises `X-GM-EXT-1` is the exception. It gets at
most one copy of each `Message-Id` in each folder. The bullet about
`X-GM-EXT-1` below describes this rule.

These limitations apply:

- **Messages without a `Message-Id` are skipped.** Drafts, some bulk mail and
  messages from broken senders may not have one. They cannot be tracked across
  servers, so they are never copied. `sync` prints a warning for each source
  folder that holds such messages, so you know how many it skips. The warning
  shows one time for each folder. A destination folder gets no warning, because
  `sync` copies nothing from it.
- **Servers that rewrite `Message-Id` on `APPEND` will cause duplicates on
  re-run.** A few IMAP servers (notably some Exchange configurations) replace
  the inbound `Message-Id` with their own value. The diff on the next run will
  see the original IDs as still "missing" on the destination and re-upload
  them. If you suspect this, do the sync in a single pass and avoid re-running
  it against the same destination.
- **Messages that share a `Message-Id` are copied by count.**
  - A destination server that advertises `X-GM-EXT-1` is the exception. The next bullet describes it.
  - The command matches each destination copy to a source copy with the same size. Among equal sizes, the lowest UID goes first.
  - A destination copy that matches no size takes the lowest free UID.
  - The sizes only choose which source copies stay in the plan. The count never depends on the sizes.
  - If the destination changes the size of a message on upload, the match uses UID order only. The count stays correct. But the command can copy a message that the destination already has and leave out a different message.
  - Releases v1.2.1 and earlier copied only one message for each `Message-Id`. After an upgrade from such a release, the command matches the destination copy to its source copy by size. Then it copies the other copies. When the sizes stay the same, the upgrade is safe.
- **A destination server that advertises `X-GM-EXT-1` gets at most one copy of each `Message-Id` in each folder.** Gmail advertises it.
  - After the login, the command asks the destination server for its capabilities. The command finds `X-GM-EXT-1` in any letter case. The capabilities of the source server do not change the plan.
  - Each destination folder gets at most one copy of each `Message-Id`. The plan takes the copy with the lowest UID. If the destination folder already holds the `Message-Id`, the plan takes no copy.
  - The check after CREATE uses the same rule.
  - Under the `will copy` line of the plan, the command prints the line `N repeated copies of these messages are left out: the destination server keeps one message for each Message-Id`. N is the number of source copies in the same folder that the plan leaves out.
    - The line counts only planned messages. It shows only when N is more than 0. The `-q` option hides it.
    - With `-V`, the line comes before the list of UIDs.
    - Under this rule, the line `N of these messages have a Message-Id that the destination folder already holds` does not show.
  - The command cannot copy a planned message that has an empty body or no body. It also cannot copy a message that the source does not send or no longer holds. For each of these, the command tries the other copies in the same source folder first. Then it tries the copies from later mappings of the same destination folder.
  - With `-V` and without `-q`, the command prints one more line after the server delimiters: `[label] The destination server advertises X-GM-EXT-1, so each destination folder gets at most one copy of each Message-Id`. `label` is the label of the destination server.
- **Some servers keep one message for each `Message-Id` but do not advertise `X-GM-EXT-1`.** Such a server accepts the upload of a second copy, but it drops the copy. So the extra copies upload again on each run, and each run reports success. Under the `will copy` line of the plan, the command prints the line `N of these messages have a Message-Id that the destination folder already holds`. N is the number of planned copies whose `Message-Id` the destination folder already holds. The `-q` option hides this line.
- **Destination folders can be different only in letter case.** For example, a `map` can name `Archive` and `archive`. A server that ignores letter case, for example Exchange, treats these names as one folder. Then a message that the two source folders share is copied more than once. Before the scan, the command prints the line `Destination folders NAMES differ only in letter case. If the destination server ignores letter case, a message that their source folders share is copied more than once.` The line shows also with `-q`. It does not show when the destination server already lists every one of these names.
- **A folder fails when the server changes its UIDVALIDITY or does not send it.** UIDVALIDITY is a number that the server sends for a folder. If it changes, the old UIDs can point to other messages. The plan holds this number for each source folder.
  - The command checks the number before each fetch of message bodies. It also checks the number when it reads the source folder again after CREATE.
  - If the number changes, or if the server does not send the number again, the folder fails with the error `source folder changed after the sync plan, run sync again`. The error ends with the old and the new number, for example `(UIDVALIDITY 5, now 6)` or `(UIDVALIDITY 5, now none)`.
  - During the copy, the command copies no more messages from this folder in this run. The messages that it already copied stay on the destination. The command counts each message that it did not copy as an error, and the run ends with exit code 1.
  - During the check after CREATE, the folder shows in the list `Folders that cannot be synced`. The run ends with exit code 1.
  - The next run scans the folder again and copies the rest. The command keeps the number only for one run.
  - A folder that sent no UIDVALIDITY when the command made the plan gets no check.
- **A message can come without a UID.** A destination copy without a UID counts as one copy. A source message that has a `Message-Id` but no UID makes the source folder fail. The folder shows in the list `Folders that cannot be synced`. The other folders still copy.
- **Two or more mappings can copy into one destination folder.**
  - For each `Message-Id`, the folder ends with the largest count of copies. This is the count that the folder already holds, or the count that one source folder holds. It is not the sum of the counts.
  - A destination server that advertises `X-GM-EXT-1` is the exception. The folder gets at most one copy of each `Message-Id`. The first mapping in the mapping order that lists the `Message-Id` copies it. The later mappings copy none.
  - The command creates the folder one time.
  - The command can fail to copy the planned message of the first mapping. For example, the copy has an empty body or no body. Or the source does not send the copy, or no longer holds it. Then the command copies the copy of a later mapping in the same run. With `X-GM-EXT-1`, the command first tries the other copies in the same source folder.
- **A message with an empty body fails on each run when no other copy can replace it.** The command never uploads an empty message. An empty message has no `Message-Id`, so a later run cannot find it on the destination. Without this rule, each run adds one more empty message. The command counts one error for the message, and the run ends with exit code 1. With `-V`, the command shows the reason: `source returned an empty body`. The message fails again on each run until the source sends a body.
- **A message for which the source sends no body fails in the same way.** The command counts one error for the message. If the source ends the fetch without an error, the command prints no reason. The next run tries the message again.
- **A planned message that the command cannot copy can download more copies than the command uses.** This applies only to a destination server that advertises `X-GM-EXT-1`.
  - The command cannot copy a planned message in these cases:
    - the message has an empty body or no body;
    - the source does not send the message;
    - the source no longer holds the message.
  - Other copies of such a message can replace it.
  - Under the copy-by-count rule, the plan gives each such message at most one other copy from each source folder.
  - The command tries the source folders that hold these copies in order. For each folder, it downloads in one stream all the copies of the messages that no earlier folder replaced, in batches of 500 messages.
  - The stream does not stop when the first copy with a usable body replaces the planned message. The other copies of this message in this folder still download.
  - The command uploads only the first copy with a usable body. The destination gets no duplicate.
  - The flag `--bps-down` limits this download, as it limits each other read from the source server.
  - The command accepts this cost. It occurs only when the command cannot copy a planned message. The command uploads none of the extra copies.

When CREATE answers that the destination folder already exists, the folder was not in the list of the destination server. The plan has no diff against this folder, so the command reads the folder again after your answer.

- The command counts the copies in the folder again, and it uses the same rules. It removes from the plan each message that a copy in the destination folder covers. It never adds a message.
- If two or more mappings share the destination folder, the command compares their plans together, in mapping order. So the folder ends with the largest count of copies, as in a run where the folder is in the list.
- When the command reads the source folder again, it also checks the UIDVALIDITY. If the number changed since the plan, or if the server did not send it, the folder fails. The command prints it in the list `Folders that cannot be synced`, and the run ends with exit code 1.
- Without `-q`, the command prints the line `Destination folder ... already exists and holds N of the M planned messages from ..., so they are not copied`.
- Without `-q`, if a planned message left the source, the command prints the line `Source folder ... no longer holds N of the M planned messages for ..., so they are not copied`.
- The command also makes this check for a folder that it creates, if the destination server sent a name that is not valid modified UTF-7.

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

- During the greeting, the login, the mailbox list and the read of the server capabilities, Ctrl+C stops the program at once.
- If a server accepts a connection but sends no greeting, the connection attempt fails after 30 seconds.
- If no data moves for 5 minutes during the login, the connection attempt fails.
- During a command, the program watches for data in both directions. If no data moves for 5 minutes, the program closes the connection and connects again. Then it sends the command one more time.
- While `sync` scans folders, creates folders and copies messages, the program prints one line before it connects again: `[label] No data from server for 5m0s, reconnecting`. The line shows also without `-V`. The `-q` option hides it.
- If the program closes the connection during the upload of a message, it does not send that message again. The next run copies it.
- A transfer that moves data has no time limit. The program does not close a connection that is idle between commands.
- When the program exits, it logs out all connections at the same time. A `sync` waits a maximum of 4 seconds for this, and a `show` waits a maximum of 2 seconds.

The 5-minute limit does not change the Ctrl+C entry above.

## License

MIT
