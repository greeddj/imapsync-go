# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Common commands

The project uses [`just`](https://github.com/casey/just) as its task runner. The `Justfile` is the source of truth for tooling invocations.

- `just deps` - `go mod tidy && go mod vendor` (deps are vendored; run after touching `go.mod`)
- `just lint` - `golangci-lint run ./... --timeout=5m`
- `just fix` - `go fmt ./...`, `go fix ./...` and `go tool fieldalignment -fix ./...`
- `just test` - `go test ./...` (run a single test with `go test ./internal/config -run TestName`)
- `just check` - runs `go vet`, `staticcheck`, `govulncheck`, and `fieldalignment` (these are declared as Go `tool` directives in `go.mod`, invoked via `go tool`)
- `just run` - `check` + `lint` + `test`, then `go run -race ./cmd/imapsync-go/main.go`
- `just build` - produces `dist/imapsync-go` (CGO disabled, trimpath, version metadata injected via `-ldflags -X main.{Version,Commit,Date,BuiltBy}`)
  - `VERSION` is the output of `git describe --tags --always --dirty`. On a tagged commit, it is the tag. After the tag, it also holds the number of commits after the tag and the commit hash. A changed tracked file adds `-dirty`. Without a tag, it is the commit hash. Outside a git repository, it is `dev`, and `COMMIT` is empty.
  - A git tag name can hold shell syntax. The `Justfile` excludes each tag with a character outside `A-Za-z0-9._+/-`. If the result still holds such a character, it uses the commit hash, with `-dirty` for a changed tracked file. The build recipes pass `LDFLAGS` to `go build` through `quote(LDFLAGS)`. So the shell expands nothing in it, and `go build` gets it as one `-ldflags` argument. Never give git output to a shell without this filter.
- `just build_linux` - Linux/amd64 cross-build, with the same version metadata
- `just oci [executor=podman] [tag=local]` - cross-builds the Linux binary, stages it under `dist/oci/linux/amd64/dist/`, then builds a `linux/amd64` image from `Dockerfile` with `dist/oci` as the build context

`fieldalignment` is enforced; if it complains about a struct, reorder fields rather than disabling the check.

Releases are produced by `goreleaser` (`.goreleaser.yml`) - do not hand-edit `dist/`.

The container image is built from the per-platform tree `dockers_v2` assembles: each binary lands at `<goos>/<goarch>/dist/imapsync-go` in the build context, following the `dist/` prefix in the build's `binary:` value, and `Dockerfile` resolves it through `ARG TARGETPLATFORM`. `just oci` reproduces the same layout under `dist/oci`. `.goreleaser.yml`, `Dockerfile` and the Justfile `oci` target share that path shape; moving one requires moving the other two. Multi-platform images are assembled in the publish phase, so on a tagged release `--skip=publish` skips the image entirely; `goreleaser release --snapshot` still builds them locally, which is how to build one by hand.

## Architecture

Single-binary IMAP-to-IMAP sync tool. Six things to internalize before making changes.

### 1. Layering

```text
cmd/imapsync-go/main.go         - urfave/cli/v3 wiring, version flags, signal-to-context (the signal becomes the cancel cause, and the exit code of a cancel reads it), the root action (it shows the help for `help [command]` and refuses an unknown command through `reportError`)
cmd/imapsync-go/commands/       - thin subcommand definitions (sync, show); they only declare flags and call into internal/app
internal/app/sync.go            - sync orchestration: ActionSync reads flags and config, runSync connects, fixes typed delimiters, refuses a typed destination name with an empty level, converts folder names, fails each derived destination name with an empty level, warns about destination names that are different only in letter case, builds the plan (matches copies by count and size, or takes one copy for each Message-Id when the destination advertises X-GM-EXT-1), confirms (the preview lists the parent folders that the run creates), creates the destination folders (a refused CREATE fails only the plans of that folder), dispatches to worker pool, routes the Client log lines to the screen in each phase (`routeClientLogs`, `lineLog`)
internal/app/show.go            - show orchestration: ActionShow reads flags and config, runShow runs parallel ListMailboxes via errgroup, formatted table (no colour codes with `--quiet`; the two tracker lines stay above the tables), the LIST names that are not valid modified UTF-7, then a list of the folders it cannot read (ErrSilentExit)
internal/app/worker.go          - syncWorker pool + runFolderSync helper consumed by sync.go (each planned message ends as copied, failed or gone), the tracker rows of the plans (`newPlanTracker`, `newStoppedPlanTracker`), standby copies after an empty body, a body that the source does not deliver or a message that the source no longer holds, the throttle stop of a run (throttleStop), the stop of a plan whose destination gave up
internal/client/client.go       - go-imap wrapper: Options, dial+TLS, guardConn wiring (watchdog, rate limit), bounded greeting and logout, reconnect with the give-up and revive gate, EnsureSession (checks the destination session without a command), New reads CAPABILITY after the first LIST (detectOneCopyPerID, KeepsOneCopyPerMessageID reports X-GM-EXT-1), Select that trusts the go-imap session state
internal/client/watchdog.go     - idleConn: inactivity watchdog for commands in flight
internal/client/fetch.go        - single-pass FetchMessageMap (Message-Id, UID and RFC822.SIZE of each copy, and the UIDVALIDITY, source; it warns about messages with no Message-Id), RescanMessageMap (the same scan with no warning, for the check after CREATE) and FetchMessageCopies (Message-Id copies, destination; it gives no such warning), CheckUIDValidity, StreamMessagesByUIDs (batched UID FETCH of the flags, the INTERNALDATE and the body, a UIDVALIDITY check before each one, a retry fetches only the UIDs not yet delivered, a refused batch costs only that batch), ExistingUIDs (UID SEARCH for the planned UIDs that the stream did not deliver, no body)
internal/client/mailbox.go      - MailboxExists / IsNoSelect / ListSubfolders / CreateMailbox via the cache (it counts the parent folders that it made), MissingLevels (the names that CreateMailbox sends a CREATE for; an empty level is no parent level), InvalidMailboxNames, ListMailboxes (a folder that cannot be read carries its reason in MailboxInfo.Err)
internal/client/cache.go        - per-Client mailboxCache (one LIST "" "*" through listAll, which leaves out names that are not valid modified UTF-7), a created mailbox is added to it, the names that LIST marks \Noselect or \NonExistent are kept, ResolveName folds a leading INBOX level, the delimiter stays empty for a flat server
internal/client/append.go       - AppendMessage streaming the literal directly from the fetched body with the flags and the INTERNALDATE of the source, one retry only before go-imap reads the body, refuses an empty or missing body before any command
internal/client/errors.go       - classifyError: Transient / Permanent / Throttled, text matched on leaf errors only; IsThrottled (the one predicate that internal/app uses); canRevive; isAlreadyExistsErr; ErrSelectRefused marks a server refusal of SELECT or EXAMINE; StreamError (the failed batches of one stream); ErrEmptyBody and ErrMissingBody mark a message that AppendMessage refuses; ErrUIDValidityChanged marks a changed source UIDVALIDITY; errMissingUID marks a scanned message without a UID
internal/client/provider.go     - DetectProvider and the provider table (the daily quotas, and rates that stay at least 5% under them for a non-stop run), used by sync.go to print quota warnings
internal/config/                - JSON+YAML config loader (extension-driven, unknown keys rejected), validation
internal/progress/              - go-pretty progress writer/tracker abstraction (interfaces consumed by client); `Writer.Stop` waits for the last render and erases nothing; every Writer has an `Indeterminate` bar indicator
internal/ratelimit/             - net.Conn wrapper applying golang.org/x/time/rate, installed by guardConn
internal/termsafe/              - Escape and Writer: make text from IMAP servers safe to print on a UTF-8 terminal (stdlib only)
internal/utils/                 - `AskConfirm(ctx, in *bufio.Reader, out io.Writer, prompt)` and friends
```

`ActionSync` and `ActionShow` are thin CLI adapters. They pass `client.Options{UseTLS: true}`, `os.Stdout` and `os.Stdin` to `runSync` / `runShow`. Tests pass plaintext options, a buffer and a scripted reader to dial the fake IMAP servers. Do not add a config, flag or environment switch that disables TLS.

`internal/client` defines minimal `ProgressWriter` / `ProgressTracker` interfaces it consumes; `internal/progress` implements them. Preserve this split when adding progress hooks - the alternative pulls `internal/progress` (which depends on go-pretty) into the client package.

`internal/termsafe` is a leaf package that uses only the standard library. `internal/client`, `internal/app` and `cmd/imapsync-go` import it. It does not import `internal/progress`.

`cmd/...` is the CLI entry point and depends on `internal/...`. Never import `cmd/` from `internal/`.

### 2. Idempotent sync via Message-Id matching (`internal/app/sync.go`)

The sync flow is **plan, confirm, execute** - not stream-as-you-go:

1. **Delimiter reconciliation and name conversion**: First, `runSync` prints one escaped warning line for each LIST name that is not valid modified UTF-7 (`printInvalidNames`). The mailbox cache leaves these names out. The source lines come first, and each side is in name order. These lines show also with `--quiet`. They do not change the exit code.
   - A typed mapping comes from the config `map` or from `--src-folder` and `--dest-folder`. `runSync` copies the config `map` before it changes a name, because the changes are in place and the config belongs to the caller.
   - Only a typed mapping goes through `offerDelimiterFixes`. A mirror run asks no delimiter question.
   - `delimiterRewrite` offers a rewrite of a typed name only when all these conditions are true:
     - the server of that side has a delimiter;
     - the mailbox cache does not list the typed name;
     - the name holds `/`, `.` or `\`, and this character is not the delimiter of the server;
     - the cache lists the name when the server delimiter replaces this character. The function tries the three characters in this order and takes the first listed name.
   - A name that the cache lists never changes, because a delimiter character can be part of a name. A typed name that the cache does not list, and that has no listed rewrite, stays as typed and gives no question.
   - `offerDelimiterFixes` shows all rewrites in one notice and asks one question. `-y/--confirm` accepts without a question. If the user declines, the names stay as typed.
   - `resolveMappings` replaces each typed name with the name that the mailbox cache lists (`Client.ResolveName`). So the conflict check, the clash check and the subfolder cut compare the bytes of the server.
   - In a typed run, `checkSourceConflicts` runs after `offerDelimiterFixes` and `resolveMappings`. Two spellings of one INBOX source count as one source.
   - Two items with one source and different destinations stop the run before the scan. The error wraps `errSourceConflict`: `map entries J and I: different destination folders for source folder NAME`. `reportError` escapes the `Error:` line. `config.New` makes a first check on the raw text (see Conventions).
   - In a typed run, `checkDestinationLevels` runs after `checkSourceConflicts` and before the expansion. It reads the typed names only. A derived name of a subfolder or of a mirror run never goes through this check. Step 2 describes the check of such a name (`splitEmptyLevelDestinations`). `checkDestinationLevels` refuses a typed destination name when all these conditions are true:
     - the destination has a delimiter. A flat destination has no levels, so it never gives this error;
     - the name starts with the delimiter, ends with the delimiter, or holds the delimiter two times in a row (`hasEmptyLevel`);
     - the mailbox cache of the destination does not list the name (`MailboxExists`, after `resolveMappings`). A listed name needs no CREATE, so the check does not refuse it. A name that LIST marks `\Noselect` counts as listed.
   - A name with no empty level gets no `MailboxExists` call. So an ended context gives its error only for a name with an empty level.
   - The check stops the run at the first item that fails, in the order of the items. CREATE cannot make a folder with an empty level, so the run stops before the scan. The error wraps `errEmptyLevel`: `map entry I: destination folder has an empty level: "NAME", destination delimiter "D"`.
     - I is the position of the item in the config `map`, counted from 1. With `--src-folder` and `--dest-folder`, the text `--dest-folder` replaces `map entry I`.
     - NAME is the typed text after `resolveMappings`. That step folds a leading INBOX level to upper case and changes no other byte of an unlisted name. `reportError` escapes the `Error:` line.
     - The run prints no plan and shows no sync prompt. The delimiter question of `offerDelimiterFixes` can come before the check. The servers see only LOGIN or AUTHENTICATE, CAPABILITY, LIST and LOGOUT. They see no EXAMINE, STATUS, FETCH, CREATE or APPEND.
   - In a typed run, `runSync` removes each repeated item after `checkDestinationLevels` (`dropRepeatedMappings`). A repeated item has the same source and the same destination as an earlier item. `dropRepeatedMappings` compares the items after the delimiter fix and `resolveMappings`. So two spellings of one INBOX source with one destination are a repeated item.
     - The first item stays, and the order does not change.
     - The delimiter notice and the checks before `dropRepeatedMappings` read all the items. So they give the positions in the config `map`. If the delimiter fix changes a repeated item, the notice shows one line for each position.
     - The config `map` of the caller does not change.
   - A mirror run builds its mappings with `mirrorMappings`. It gives each listed source name to `convertFolderPath` and asks no question.
   - `convertFolderPath(path, srcDelimiter, dstDelimiter)` gives the destination name. These rules apply in this order:
     - If the destination has no delimiter, or the two delimiters are the same, the path does not change.
     - If the path holds no text of the destination delimiter, each source delimiter becomes the destination delimiter.
     - If not, the destination delimiter inside a level is name text. The function replaces it with `_` (with `-` when the destination delimiter is `_`) and joins the levels with the destination delimiter. The result is a renamed name (`folderMapping.renamed`).
   - A typed destination is never converted. The relative path of each subfolder gets the conversion. A subfolder is renamed when this conversion renamed it or when its root is renamed.
2. **Expand mappings**: only a typed run expands its mappings with subfolders. `expandMappingsWithSubfolders` calls `client.ListSubfolders`, which serves the answer from the per-Client mailbox cache (one `LIST "" "*"` populates it on first connect). A mirror run uses `expandMirrorRoots` and walks no subfolder (see below).
   - The expansion skips each source name that `Client.IsNoSelect` reports. This applies to the mapped name and to each subfolder.
   - `IsNoSelect` reads the mailbox cache. The cache marks a name when the same `LIST "" "*"` gives it the `\Noselect` or `\NonExistent` attribute. The attribute can have any letter case.
   - The expansion keeps the subfolders of a skipped name.
   - `expandMappingsWithSubfolders` prints nothing. It returns the expanded mappings and an `expansionReport`. The report holds the entries, the winner of each entry and the skipped names. `expandMirrorRoots` also prints nothing, and its report holds only the skipped names.
   - A mapped source name that LIST does not show stays in the expansion, also when LIST shows its subfolders. Step 3 scans it.
   - A flat source has an empty delimiter. The expansion lists no subfolder for it, so each mapping gives one folder.
   - The expansion sorts the subfolders of each mapping in byte order, because the cache lists names in map order. So the plans and the failure lines are the same on each run.
   - The relative path of a subfolder is the part of its listed name after the mapped name and one delimiter. The code cuts this part by length, because the listed name can spell a leading INBOX level in another letter case.
   - If the destination has a delimiter, `convertFolderPath` (step 1) converts the relative path. The destination name of the subfolder is then the destination root, the destination delimiter and the converted path. A flat destination has no hierarchy, so the name is the destination root, the source delimiter and the relative path.
   - Inside the expansion, `dstClient.ResolveName` gives each mapped and each derived destination the listed spelling of its INBOX level.
   - `mostSpecific` gives each source name one entry. The entry of the mapping whose source is the name itself, or its nearest ancestor, wins. This is the longest root.
     - When two roots have the same length (two listed spellings of one INBOX level), the smaller root in byte order wins.
     - The order of the map items does not change the result. A mirror run does not use `mostSpecific`.
   - A mirror run does not expand its mappings with subfolders. `expandMirrorRoots` gives one mapping for each listed source name, because each listed name is a root and its own mapping wins over each derived entry.
     - `runSync` gives the function the output of `mirrorMappings`. The destination name comes from `convertFolderPath` (step 1), with the `renamed` flag of that conversion. `dstClient.ResolveName` then gives the destination the listed spelling of its INBOX level.
     - The function skips each source name that `Client.IsNoSelect` reports, with the same reason as the typed expansion. It keeps the order of `MailboxNames`, which is byte order.
     - The function calls neither `ListSubfolders` nor `mostSpecific`. It sends no command and reads only the mailbox cache. The cost is linear in the number of listed names. If the context ended, it returns the context error.
     - The report holds only the skipped names.
     - The result is the same as the result of `expandMappingsWithSubfolders` for the same mappings.
     - A subfolder walk of the mirror mappings makes one entry for each pair of a mapping and a listed descendant. For a chain of depth d, that is about d*d/2 entries with about d*d*d bytes of names. `mostSpecific` keeps only the d entries of the roots.
   - `splitDestinationClashes` groups the expanded mappings by destination. When a group holds two or more mappings and at least one is renamed, every mapping of the group fails. Its `FolderFailure` wraps `errDestinationClash` and names one other source folder of the group. A group with no renamed mapping stays unchanged.
   - `splitEmptyLevelDestinations` runs after `splitDestinationClashes` and before the scan. It checks the destination name of each planned mapping. So it covers the names that the code builds, for example the subfolders of a mapping and the names of a mirror run. A mapping fails when all these conditions are true:
     - the destination has a delimiter. A flat destination has no levels, so it never gives this failure;
     - the name starts with the delimiter, ends with the delimiter, or holds the delimiter two times in a row (`hasEmptyLevel`). A source folder such as `x/` or `a//b` gives such a name;
     - the mailbox cache of the destination does not list the name (`MailboxExists`). A listed name needs no CREATE, so its mapping stays. A name that LIST marks `\Noselect` counts as listed, and the rule for a `\Noselect` destination (step 3) decides its result.
   - A server can read a CREATE of `x/` as a CREATE of `x`, but the APPEND commands name `x/`. So a mapping with such a name fails before the scan, in the same way as a clash.
     - The `FolderFailure` wraps `errEmptyLevel`. The text is `destination folder has an empty level: "NAME", destination delimiter "D"`. It has no `map entry I` prefix, because the failure line names the source folder and the destination folder.
     - NAME is the destination name that the code built. The code builds it from a source folder name, so the name is server text. `printFolderFailures` escapes it.
     - The mapping gives no plan, so no command for that folder reaches either server. The servers get no EXAMINE, STATUS, CREATE or APPEND that names it.
     - In step 1, `checkDestinationLevels` refuses each typed name that fails this rule. So only a derived name fails here. The other mappings go on.
     - A name with no empty level gets no `MailboxExists` call. So an ended context gives its error only for a name with an empty level, and `runSync` returns that error.
   - `runSync` puts the clash failures first, then the empty-level failures, then the failures of the scan in `SyncSummary.Failures`. A clash or an empty level gives no plan, so the run creates no destination folder for it. `printFolderFailures` shows the failure after the scan, also with `--quiet`. The run ends with exit code 1 (`ErrSilentExit`), as for each folder failure (step 3).
   - With `--verbose` and without `--quiet`, `runSync` prints the line `Checking for subfolders...` before the expansion in a typed run. A mirror run prints no such line. `printExpansion` prints the other lines of the expansion. It prints them only with `--verbose` and without `--quiet`.
   - `runSync` calls `printExpansion` after `splitDestinationClashes` and `splitEmptyLevelDestinations`, and before `printFolderRenames`. The function gets the planned mappings, which are the mappings that remain after both splits. It prints these groups in this order:
     - one escaped line, two spaces and then `📁 Found subfolder: %s → %s`, for each entry, in the order of the entries, when all these conditions are true:
       - the entry is a subfolder of its root, so its source name is different from its root;
       - a planned mapping names the source folder of the entry;
       - the destination of the entry is the destination of its winner;
     - one escaped line, two spaces and then `ℹ️  Folder %s skipped - %s`, for each skipped name, in name order;
     - one line for each entry that lost, in the order of the entries, when a planned mapping names the source folder of the entry:
       - an entry with another destination gives the escaped line two spaces and then `ℹ️  Folder %s goes to %s, not to %s: the mapping of %s wins over the mapping of %s`;
       - an entry with the same destination gives the existing `%q` line `Mapping ... skipped - already covered by a parent mapping's expansion`;
     - the line `📂 Found %d subfolders, total folders to sync: %d`, only when the first number is more than 0.
   - The planned mappings change the found-subfolder lines, the lines of the entries that lost and the count line. The `Folder ... skipped` lines do not use them. A failure after the splits, such as a failed scan, changes no line, because `printExpansion` runs before the scan.
   - An entry that lost has the same source folder as its winner, and the expansion keeps one mapping for each source folder. So the condition is true only when the winner is in the plan.
   - So a found-subfolder line never names a destination that lost. In a found-subfolder line, a `goes to` line and a `Mapping ... skipped` line, the first folder never failed in a split. The folder-failure list names that folder and the reason.
   - A `goes to` line can name a root whose own mapping failed in a split, in a part `the mapping of %s`. That mapping still gives the destination of each subfolder for which it wins.
   - A mirror run has no entry in its report. So `printExpansion` prints no found-subfolder line, no `goes to` line, no `Mapping ... skipped` line and no count line. It prints only the `Folder ... skipped` lines.
   - The first number of the count line is the number of entries for which all these conditions are true:
     - the entry wins (`mostSpecific`);
     - the entry is a subfolder of its root;
     - a planned mapping names the source folder of the entry.
   - The second number of the count line is the number of planned mappings. A subfolder with a mapping of its own is a root, so the first number leaves it out.
   - The code counts these entries. It does not subtract the number of roots from the number of mappings. A `\Noselect` root has no entry. If the code subtracts, the count is wrong when a root is `\Noselect`. A repeated item gives no second root (step 1). So its found-subfolder lines show one time, and it gives no `Mapping ... skipped` line.
   - `printFolderRenames` prints one escaped warning line for each renamed mapping that remains after both splits, before the scan. The line shows the two names and the destination delimiter. It shows also without `--verbose` and with `--quiet`.
   - `printCaseVariantDestinations` prints one escaped warning line for each group of destination names that are different only in letter case. It runs after `printFolderRenames` and before the scan, in typed runs and in mirror runs. It reads the planned mappings, which come after `splitDestinationClashes` and `splitEmptyLevelDestinations`.
     - A group holds destination names that are equal after `strings.ToLower` but have different bytes. INBOX levels already have the listed spelling, so INBOX spellings are not variants.
     - A group gets no line when the destination LIST (`MailboxNames`) holds every name of the group, because that server keeps the names apart.
     - The function finds a name of a group in the LIST with `slices.BinarySearch`, so it relies on `MailboxNames` returning the names in byte order. It sorts each group one time and removes exact repeats (`slices.Compact`).
     - So the cost is O(n log n) for n names. The destination names come from the source server in a mirror run and for the subfolders of a typed mapping. So a hostile server can list many spellings of one name.
     - The groups come in the order of the lower-case key, and the names of a group in byte order. The line joins the names with ", ". One empty line follows the groups.
     - The line is `⚠️  Destination folders %s differ only in letter case. If the destination server ignores letter case, a message that their source folders share is copied more than once.` It shows also without `--verbose` and with `--quiet`.
     - The plans, the prompt and the exit code do not change. An example of a server that ignores letter case is Exchange.
3. **Build sync plan** (`buildSyncPlan`): two goroutines run under an `errgroup` - one walks every src mapping calling `FetchMessageMap`, the other walks every dst mapping calling `MailboxExists` + `FetchMessageCopies`. Trackers advance independently so a slow dst does not stall the src progress bar. After both sides complete a slot, `maybeDiff` fires (guarded by `atomic.Int32` reaching 2 + a `sync.Mutex` for race-detector cleanliness), computes the diff by the count of copies for each Message-Id (the one-copy rule below replaces the count for a destination that advertises X-GM-EXT-1), and immediately frees the maps to keep peak memory proportional to one folder at a time. All fetches are envelope-only via `BODY.PEEK[HEADER.FIELDS (MESSAGE-ID)] + UID + RFC822.SIZE`; bodies are not pulled at planning time. The "what would be synced" preview is printed and confirmed before any writes. `--quiet` hides the preview and the prompt, so `runSync` refuses `--quiet` without `-y/--confirm` before it connects to a server.
   - The destination goroutine calls `IsNoSelect` after `MailboxExists`. It does not call `FetchMessageCopies` for a `\Noselect` destination.
   - `FetchMessageMap(ctx, folder)` scans the source and returns `map[string][]MessageRef`, the UIDVALIDITY and an error. `FetchMessageCopies(ctx, folder)` scans the destination and returns the map and an error. Both use `scanMessageIDs`.
     - One scan sends one FETCH of `BODY.PEEK[HEADER.FIELDS (MESSAGE-ID)]`, `UID` and `RFC822.SIZE`. A `MessageRef` holds the UID and the size of one copy.
     - Each map value lists every copy of the Message-Id in ascending UID order.
     - Only `FetchMessageMap` warns about messages with no Message-Id. After a successful scan, it logs one line with the escaped folder name: `[label] ⚠️  FOLDER: N message(s) without Message-Id will be skipped - sync cannot track them`. `--quiet` hides the line.
     - A scan that fails, or that the context ends, logs no warning. After a retry on a new session, the line shows the count of the attempt that succeeded.
     - `RescanMessageMap` is the same scan with no warning. The check after CREATE (step 4) uses it, because the planning scan of the same Client already warned. It still logs the fetch, reconnect and watchdog lines.
     - So a source folder gets this warning one time at most in a run, also when the check after CREATE reads the folder again.
     - `FetchMessageCopies` logs no such line, because the program copies nothing from the destination.
   - A scan response can have a Message-Id but no UID, or UID 0. go-imap sends UID 0 as `*` in a UID set.
     - `FetchMessageMap` scans the source. It fails the scan for such a response, and the error wraps `errMissingUID`. The client does not reconnect for this error.
     - `FetchMessageCopies` scans the destination. It keeps such a response as a copy with UID 0, because the diff reads only the count and the sizes of the destination copies.
     - The scan counts each response with UID 0 as one copy, also when two such responses have the same Message-Id. A repeated response with the same UID that is not 0 counts one time.
   - The count rule: take one Message-Id. The source holds N copies and the destination holds D copies. The plan copies max(N - D, 0) source copies. The program never deletes a copy on the destination.
   - `copyMatcher.match` chooses which source copies the destination copies match:
     - each destination copy matches a source copy with the same RFC822.SIZE. Among equal sizes, the lowest UID goes first;
     - a destination copy that matches no size takes the lowest free UID;
     - so the sizes only choose the copies, and the counts never depend on the sizes;
     - the sorting keeps the cost at O(k log k) for k copies of one Message-Id, which stops a quadratic cost on a hostile server.
   - The copy rule of the sync plan is the count rule, or the one-copy rule for a destination that advertises X-GM-EXT-1 (Gmail does).
     - `buildSyncPlan` calls `copyRuleOf(dstClient)` one time. The result is `copyOnePerMessageID` when `KeepsOneCopyPerMessageID()` is true, and `copyByCount` if not. Only the planning destination Client sets the rule. The source flag never changes the sync plan.
     - Under `copyOnePerMessageID`, `copyRule.sourceCopies` cuts each Message-Id group to its lowest UID, because the scan sorts the copies by ascending UID. The count rule then runs on that one copy, so a destination folder that holds the Message-Id gets no copy.
     - `maybeDiff` gets the rule from `buildSyncPlan` and stores it in `FolderSyncPlan.copyRule`. `destinationDiff` reads `plan.copyRule`. So the sync plan and the check after CREATE (step 4) use the same rule.
     - A shared destination keeps one copy. The first mapping in mapping order that lists the Message-Id plans the copy. A later mapping pairs its copy with the reserved copy and plans none.
     - When a plan plans the copy of a Message-Id, the cut copies from the folder of the plan become standbys of that copy. `maybeDiff` stores them in `FolderSyncPlan.standbys`, in UID order, also when the destination is not shared. `mergeStandbys` puts them before the standbys of later mappings (step 5). A plan that copies by count has none.
     - `FolderSyncPlan.RepeatedCopies` counts the source copies of the same folder that the rule cut, for the planned Message-Ids only. It is always 0 under the count rule.
     - The preview prints one line under the "will copy" line, only when the count is more than 0 and only without `--quiet`. The line comes after the `ExtraCopies` line and before the verbose UID list.
     - The format is two spaces and then `ℹ️  %d repeated copies of these messages are left out: the destination server keeps one message for each Message-Id`.
     - `ExtraCopies` is always 0 under the one-copy rule. So the two lines never show for the same plan.
     - With `--verbose` and without `--quiet`, `runSync` prints one line after the "Server delimiters" lines and their empty line, when the destination Client advertises X-GM-EXT-1. An empty line follows it.
     - The format is two spaces and then `ℹ️  [%s] The destination server advertises X-GM-EXT-1, so each destination folder gets at most one copy of each Message-Id`. `%s` is the destination label, which is operator text, so the line needs no escape.
   - `NewSize` adds up the RFC822.SIZE of the planned copies only.
   - Two or more expanded mappings can have one destination. This applies to typed runs, mirror runs, and an expanded subfolder plus a mapping. `sharedDestinations` tracks them, and `buildSyncPlan` compares their scans in mapping order.
     - Each mapping except the last of its destination reserves its planned copies for each Message-Id. A `reservation` holds the UID, the size and the slot.
     - A later mapping first removes the copies that the destination holds, with the count rule. Then it pairs the copies that are left with the reserved copies, with the same size rule.
     - A paired copy is not planned. A copy with no pair is planned, and it is also reserved when the mapping is not the last one.
     - So the destination ends with the largest count of copies, which is its own count or the count of one source folder. It is not the sum.
     - `maybeDiff` releases the reservations of a destination after it compares the last mapping of that destination, also when that mapping fails.
     - A paired copy from another source folder becomes a standby of the reserved copy (`FolderSyncPlan.standbys`). A `standbySource` holds the folder and its UIDVALIDITY.
     - Under the one-copy rule, the copies that the rule cut from the folder of that later mapping also become standbys of the reserved copy.
     - The preview list "Folders to be created on destination" names a shared destination one time, and the program sends one CREATE for it.
   - `FolderSyncPlan.ExtraCopies` counts the planned copies whose Message-Id the destination folder already holds. Copies that an earlier mapping of a shared destination reserves in the same run do not count.
     - The preview prints one line under the "will copy" line, only when the count is more than 0 and only without `--quiet`. With `--verbose`, the line comes before the UID list.
     - The format is two spaces and then `ℹ️  %d of these messages have a Message-Id that the destination folder already holds`.
     - A server that keeps one message for each Message-Id but does not advertise X-GM-EXT-1 accepts a duplicate APPEND and drops the copy. So these copies upload again on each run, and each run reports success. The line warns about this.
     - The one-copy rule makes this line never show, because the plan takes no copy of a Message-Id that the destination folder holds.
   - `buildSyncPlan` stores the UIDVALIDITY of each source scan in `FolderSyncPlan.SrcUIDValidity`. The value is 0 when the server sent none, and a plan with 0 is never checked.
     - `FetchMessageMap` takes the value from the EXAMINE. When the folder stays selected and no EXAMINE runs, it takes the value from `STATUS (MESSAGES UIDVALIDITY)`. A retry on a new session returns the value of the new session.
     - The value exists only in the sync plan of one run. It is not a record that stays between runs.
   - A failed source scan gives a folder failure (`FolderFailure` in `SyncSummary.Failures`). Its error wraps `errSourceScan` ("cannot scan source folder").
   - A source mapping is a skip (`FolderSkip` in `SyncSummary.Skipped`), not a folder failure, when all these conditions are true:
     - the scan error wraps `client.ErrSelectRefused`. `FetchMessageMap` adds this marker only when the server refuses the folder with a reply that is not transient and not throttled;
     - the name is not INBOX, in any letter case;
     - the mailbox cache does not hold the name;
     - the mailbox cache holds at least one subfolder of the name (`isHierarchyLevel`).
   - A skip does not change the exit code.
   - A `\Noselect` destination is a folder failure when the source scan succeeds and finds at least one message with a Message-Id. When the source scan succeeds and finds no such message, the mapping gives no plan and no folder failure. When the source scan fails, the source rules above apply, and the destination gives no second folder failure.
   - The error of a `\Noselect` destination failure wraps `errDestinationNoSelect`.
   - A scan failure of a destination that is not `\Noselect` stops the run before any write.
   - The scan has its own `progress.Writer` with one tracker for each side. Both planning Clients get this writer and the tracker of their side.
   - When the scan ends, `runSync` marks both trackers done and calls `Stop` on the writer. `Stop` erases nothing, so the Message-Id warnings and the two scan lines stay on screen. Then `routeClientLogs` gives both planning Clients the `lineLog` writer and no tracker (Section 3).
   - After the scan, `runSync` does these steps in this order, before the plan preview and the prompt:
     - if the context ended, it prints nothing and returns the context error;
     - without `--quiet`, it prints one escaped line for each skip;
     - it prints the folder-failure list, also with `--quiet`.
   - A folder failure gives no plan, so the run creates no destination folder for it.
   - When the plan has no message to copy and a folder failure exists, the run shows no prompt. It prints the error summary line and exits with code 1 (`ErrSilentExit`).
   - After the copy, when a folder failure exists, the run exits with code 1 (`ErrSilentExit`). Without `--quiet`, it first lists the folder failures again. It always prints the error summary line, with the count of failed folders.
4. **Pre-create destination folders** for the active plans (sequential, with per-folder mutex on `Client.folderLocks`). New mailboxes are added back into the cache. A name that LIST marks `\Noselect` counts as existing, so the client sends no CREATE for it, also when it is a parent.
   - The creation stage creates a destination folder only for a plan, and the missing parent levels of that folder. This behaviour is on purpose: `sync` copies messages, not empty folders. Do not add a CREATE for a mapping that has no plan.
     - `maybeDiff` makes a plan only when it plans at least one UID (step 3). So a source folder with no message to copy gets no CREATE.
     - These source folders have no message to copy:
       - an empty folder;
       - a folder in which no message has a Message-Id, because the scan skips such messages;
       - a folder whose messages the destination folder already holds, when the destination LIST shows that folder.
     - A destination folder that LIST hides gets no destination scan, so `maybeDiff` compares the source with an empty destination. The run sends a CREATE for it. When the CREATE reply says that the folder exists, the check after CREATE removes from the plan the messages that the folder holds.
     - When `TotalNew` is 0, `runSync` returns through `endRunWithoutCopy` before this stage. Without failures and without `--quiet`, it prints `✅ All folders already synced!`.
     - A mapping with no plan gets its destination folder in two cases. The folder is a missing parent level of a planned folder (`MissingLevels`), or another mapping with the same destination has a plan (`sharedDestinations`, step 3).
     - A later run creates the folder when the source folder gets a message to copy. The program keeps no state between runs.
     - `TestRunSync_When_SourceFolderHasNoMessageToCopy_Then_NoDestinationFolderIsCreated` tests this behaviour in a mirror run. `TestRunSync_When_TypedMappingsHaveAnEmptySourceFolder_Then_OnlyFoldersWithMessagesGetADestination` tests it in a typed run.
   - `destinationsToCreate` and `foldersToCreate` list the names for which the creation stage sends a CREATE. The preview, the creation counter and the CREATE commands use one order. So all three agree while the server accepts each CREATE.
     - `destinationsToCreate` takes the destination folder of each plan that copies a message to a folder that the plan did not find. It sorts the folders in byte order and keeps each folder one time. `runSync` calls it one time and keeps the result as `createTargets`. Byte order puts a parent path before its children.
     - `foldersToCreate` takes `createTargets`. For each folder, in this order, it asks `Client.MissingLevels`.
     - The creation loop walks the same `createTargets`. For each folder, `CreateMailbox` creates the missing parent levels from the top, then the folder. So the CREATE commands follow the order of the list.
     - `MissingLevels` returns the parent levels that the mailbox cache does not list, from the top, and then the folder. For a cached folder, it returns nothing. A name that LIST marks `\Noselect` counts as listed.
     - A flat destination has no parent level, so the list holds the folder only.
     - `parentLevels` gives the parent levels. A prefix of the name is a parent level when it ends before a delimiter. Its last level must not be empty, because an empty level names no mailbox.
     - So `/X` has no parent level, `a//b` has the parent level `a`, and `x/` has the parent level `x`. The folder keeps its name.
     - On a destination with a delimiter, a name with an empty level gives no CREATE. For a typed name, `checkDestinationLevels` refuses it, or the cache lists it (step 1). For a derived name, `splitEmptyLevelDestinations` fails its mapping, or the cache lists it (step 2). A derived name is the name of a subfolder or a mirror name. A flat destination has no levels, so it gets one CREATE for the whole name.
     - Let L be the length of a folder name in bytes. A name has a maximum of L/2 parent levels, rounded down. Each parent level needs a last level and a delimiter, and each of them has 1 byte or more.
     - So one folder adds a maximum of L/2 + 1 names to the list, and a full list of these names prints O(L^2) bytes. The names come from the source server in a mirror run and for the subfolders of a typed mapping. So a hostile server can list a long name with many levels.
     - The list holds each name one time. A level that two folders share shows at the first folder that needs it. When the context ends, the function returns the context error, and the run prints no preview.
     - `printFoldersToCreate` shows the first `previewFolderLimit = 20` names. If the list holds more names, it then shows the line `• … and N more`. The limit applies to the preview only. The creation stage still sends a CREATE for each name.
   - The creation tracker counts the names of this list:
     - Its total is the length of the list. Before each `CreateMailbox` call, the text is `(K/T) Creating NAME`. K is one more than the sum of `parents + 1` for the earlier calls. It never exceeds T.
     - `CreateMailbox` returns `(created, parents, err)`. `parents` counts the parent folders for which the server accepted a CREATE, also when `err` is set. `parents` does not count a parent that the server reports as existing.
     - `runSync` adds `parents` to the count of created folders. After each `CreateMailbox` call, it advances the tracker by `parents + 1`. The throttled reply that ends the loop is the exception.
     - The tracker sets `AutoStopDisabled`, so the loop decides the final state. If the server refused no folder, the text is `Created N folders` with the done mark. If not, the text is `Created N of T folders, R refused` with the error mark. A throttled refusal counts as refused.
     - `routeClientLogs` gives the destination planning Client the creation writer. When the loop ends, `Stop` keeps the last tracker line and each log line on screen. Then `routeClientLogs` gives the Client the `lineLog` writer again.
   - A folder that the server refuses to create fails only the plans of that folder:
     - `runSync` keeps the error of each refused folder. A throttled reply is no refusal of the folder (see the next item).
     - After the creation loop, `splitRefusedDestinations` takes each plan with a refused destination out of the active plans. It makes a `FolderFailure` for each of them. The error wraps `errDestinationCreate`. The other plans stay.
     - If more than one mapping shares a refused destination, every one of these mappings fails.
     - `CreateMailbox` creates the parents of a folder before the folder. A refused parent fails each folder below it, because the CREATE of such a folder sends the CREATE of the parent again. The error then reads `failed to create parent folder NAME`.
     - `runSync` adds the failures to `SyncSummary.Failures` and prints them at once with `printFolderFailures`, also with `--quiet`. The run goes on with the other plans and ends with exit code 1 (`ErrSilentExit`).
     - The log `Failed to create folder` shows the reply of the server. `--quiet` hides the log.
     - If no plan remains, the run opens no worker connection. It ends in the same way as the case with no message to copy.
   - A throttled reply to CREATE stops the run (step 5):
     - `runSync` records the stop and sends no more CREATE. The folder with the throttled reply is no folder failure. The folders that the server refused before keep their failures.
     - The run sends no command for the check after CREATE and starts no worker.
     - If the context did not end, `runSync` counts each message of the plans that remain as an error. It prints the folder-failure list (not with `--quiet`, because `runSync` printed the list already), the stop line and the error summary line. It ends with exit code 1.
   - `CreateMailbox` gives `created == false` and no error when the CREATE reply says that the folder exists, or when the mailbox cache already holds the name. The planning scan did not find that folder, so its plan has no diff against the folder. `runSync` keeps each such folder in a set.
   - If the destination LIST has names that are not valid modified UTF-7 (`InvalidMailboxNames`), the set also holds each folder that CREATE made. An OK reply does not prove that the folder is new.
   - After the creation loop and before the worker pool, `recheckExistingDestinations` checks each plan whose destination is in this set. It runs in sequence on the two planning clients, sends no LIST and starts no goroutine. All checks end before the first APPEND.
   - `recheckExistingDestinations` groups these plans by destination, in the order of the first plan of each destination. Inside a group, the plans keep the mapping order. `recheckDestination` checks one group.
   - `recheckDestination` reads the destination one time with `FetchMessageCopies`. If the folder holds no Message-Id, the plans do not change and the run sends no source command.
   - If not, `recheckDestination` reads each source again with `RescanMessageMap`, in mapping order. After each scan, it calls `client.CheckUIDValidity` with the value of the plan (step 5). Only one source map exists at a time.
   - A plan whose source scan fails is a folder failure. A plan whose UIDVALIDITY changed is also a folder failure. The other plans of that destination continue without it.
   - `destinationDiff` checks the plans of one destination together. `addSource` pairs the copies of each source with the copies of the sources before it. It uses the copy rule (`plan.copyRule`) and the size rule of `maybeDiff`, with an empty destination.
   - Then `results` lets the destination copies cover the copies that remain. It uses the same `copyMatcher` and the destination sizes only.
   - Only planned UIDs stay. The check never adds a UID to a plan and never shows a second prompt. When the sources do not change, the destination ends with the largest count of copies of each Message-Id (step 2).
   - For a destination with one plan, the check keeps the UIDs that `maybeDiff` plans with the same destination copies. With two or more plans, the check can keep other copies of a Message-Id, but the count of copies is the same.
   - `results` sets `NewSize`, `ExtraCopies` and `RepeatedCopies` again for the kept UIDs. The run prints none of these values after the prompt.
   - The two counts of `recheckDrops` are `held` and `gone`. `held` counts the dropped UIDs that the source still lists. `gone` counts the dropped UIDs that the source no longer lists. The check counts each planned UID one time, so `held`, `gone` and the kept UIDs add up to the planned UIDs.
   - Without `--quiet`, `runSync` prints up to two escaped lines for each plan that loses messages. The lines print after all checks end, in plan order. M is the number of planned messages in both lines. Each line shows only when its count is more than 0.
     - First line, for `held`: `Destination folder ... already exists and holds N of the M planned messages from ..., so they are not copied`.
     - Second line, for `gone`: `Source folder ... no longer holds N of the M planned messages for ..., so they are not copied`.
   - With `--quiet`, the check prints neither line. The check removes a plan that has no UID left.
   - A scan that fails gives a folder failure. Its error wraps `errDestinationRecheck` for the destination or `errSourceScan` for the source. A failed destination read gives a failure for each plan of that destination. `runSync` adds the failures to `SyncSummary.Failures` and prints the new failures at once, also with `--quiet`. The run ends with exit code 1 (`ErrSilentExit`).
   - A UIDVALIDITY failure of the source is also a folder failure. Its error wraps `client.ErrUIDValidityChanged`, not `errSourceScan`. It shows in `Folders that cannot be synced`, also with `--quiet`, and the run ends with exit code 1.
   - When the check leaves no plan, the run prints no "Syncing messages" line and opens no worker connection. It ends in the same way as the case with no message to copy.
   - When the context ends during the check, `runSync` returns the context error. It prints no line, records no failure and sends no APPEND. `FetchMessageCopies` and `RescanMessageMap` return the context error before they send a command.
   - A throttled reply to the read of the destination, or to the scan of a source, stops the run, as a throttled CREATE does. `recheckDestination` records the stop and returns no result. `recheckExistingDestinations` then returns the plans as they were, with no failure and no line, and sends no more command. `runSync` counts each message of these plans as an error.
5. **Execute via worker pool**: `newSyncWorkerPool` builds N persistent workers (each owns one src + one dst `Client` for the entire sync). The dispatch loop gives each plan to a free worker through a chan-of-workers semaphore. `computeEffectiveWorkers` gives the smallest of `--workers`, `max_connections` (when it is more than 0) and the number of plans, and at least 1. So a worker connects only when the run has a plan for it. A single `progress.Writer` covers the whole copy. A worker updates the tracker of its plan at most one time in 100 ms.
   - The planning Clients do not count against `max_connections`, and `computeEffectiveWorkers` keeps no slot for them.
     - Before `newSyncWorkerPool`, `runSync` logs out the two planning Clients at the same time through `logoutAll`. No later phase uses them, and a worker never reuses them.
     - So `max_connections` counts only worker sessions, and `--max-connections=N` gives at most N workers.
     - A run that ends before the copy logs out the planning Clients through a deferred call. This call and the logout before the pool share one `sync.OnceFunc`, so each planning session gets one LOGOUT.
   - At the end of the copy, `workers.close` logs out all workers at the same time through `logoutAll`. So a silent server costs at most two logout limits: one before the pool and one at the end.
   - The dispatch loop makes the tracker of a plan only when a worker takes the plan. So one redraw holds at most one running row for each worker. A finished tracker keeps its last line on screen. go-pretty has no limit by terminal height. If each plan has a tracker from the start, a redraw can have more rows than the terminal. Then the scrollback fills with copies of these rows.
     - `newPlanTracker` builds the tracker. It sets `AutoStopDisabled`, because `runFolderSync` sets the final text on every path and the last `Increment` must not end the row before that text.
     - After a throttle stop, `runSync` appends one row for each plan that did not start. `newStoppedPlanTracker` builds the row with the `Stopped` text and the error mark. It sets `DeferStart`, because `AppendTracker` restarts a tracker and clears its marks.
     - `runSync` calls `Stop` on the copy writer after the stopped rows. It prints the folder-failure list, the stop line, the error summary line and the success line only after `Stop`. The last redraw can erase a line that appears before `Stop`. `Stop` erases no row, so these lines stay on screen.
     - Both Clients of each worker get the copy writer and no tracker (`routeClientLogs`). So the trip line of the watchdog shows during the copy. With `--verbose`, the reconnect lines and the append lines show too (Section 3).
   - The copy checks the UIDVALIDITY. `runFolderSync` gives `SrcUIDValidity` to `StreamMessagesByUIDs`, which checks it on every attempt of every batch (Section 3).
     - If the values are different, the worker logs `Stream error for folder ...` with the escaped error. Each planned message that the stream did not deliver counts as one error (see the accounting items below). The run exits with code 1.
     - The batches before the change stay copied, and the run copies no more messages from that folder. The next run scans again and copies the rest.
     - `CheckUIDValidity(planned, current)` returns nil when `planned` is 0 or when the two values are equal. When `current` is 0, it returns an error that wraps `ErrUIDValidityChanged` with the suffix `(UIDVALIDITY N, now none)`. In all other cases, the suffix is `(UIDVALIDITY N, now M)`.
     - A session that sends no UIDVALIDITY cannot prove that the planned UIDs still hold. So a plan value that is not 0 fails against a current value of 0.
   - Each planned message ends one time, as copied, as an error or as gone. `runFolderSync` returns `(synced, errors, gone)`. These three counts add up to the planned messages, unless the context ends.
     - `StreamMessagesByUIDs` gives a message to the callback only when the response holds the body. A response without a body, for example a flag update, never reaches the callback and does not mark the UID.
     - The callback marks the UID as delivered, then calls `AppendMessage`. A refused APPEND counts as one error. If `AppendMessage` refuses the body as empty or missing, the UID waits for a standby when the plan has one.
     - After the main stream, the worker checks each planned UID that the stream did not deliver. `Client.ExistingUIDs` sends UID SEARCH on the source session, in batches of `uidFetchBatchSize`. It reads no body. It checks the UIDVALIDITY of the plan, and it removes UID 0 from the request.
     - If the source still holds the UID, the stream did not deliver the message. The cause can be a refused batch, a UID that UID FETCH left out, or a response without a body. If the source does not hold the UID, the message is gone.
     - A UID with a standby waits in `pending` with its cause. A UID with no standby ends at once. A message that the stream did not deliver counts as an error. A gone message counts as gone.
     - After the standby streams, a UID that still waits ends in the same way. A refused body and a message that the stream did not deliver count as errors. A gone message counts as gone.
     - A check that fails with a reply that is not throttled proves nothing. Then every UID of the call counts as still held. Each of them ends as an error, unless a standby replaces it. A throttled reply to the check records a stop.
     - A gone message is no error and does not change the exit code. For each plan with gone messages, `runFolderSync` prints one escaped line after the standby streams: two spaces and then `ℹ️  Source folder %s no longer holds %d of the %d planned messages for %s, so they are not copied`. `--quiet` hides the line.
     - The error summary line counts `synced` and `errors` only.
   - A refused UID FETCH batch does not end the stream:
     - `StreamMessagesByUIDs` goes on with the next batch and returns a `*StreamError` at the end. `First` is the first failure, `Stop` is the failure that ended the stream, if any, and `FailedBatches` counts the failed batches.
     - The stream ends at once only for a refused EXAMINE (`ErrSelectRefused`), a changed UIDVALIDITY or a throttled reply. A refused EXAMINE covers the whole folder, and a changed UIDVALIDITY makes the planned UIDs unsafe.
     - The worker prints one `Stream error for folder ...` line for each plan. The line shows the first failure of the plan. It ends with `(N batches failed)` when more than one batch failed in all the streams of the plan. `--quiet` hides the line.
     - The messages of a failed batch that the stream did not deliver go through the check above. A refused batch never counts as one error for many messages.
   - `AppendMessage` refuses a message with an empty or missing body (Section 5). `runFolderSync` handles the refusal in this way:
     - if the plan has standbys for the refused UID (`FolderSyncPlan.standbysOf`), the UID waits;
     - after the main stream, the worker streams the standby UIDs from each standby source, in the order of `FolderSyncPlan.standbySources`. It appends the first copy that has a usable body. A standby stream uses the UIDVALIDITY of its own standby source;
     - when the plan has standbys from its own folder, that folder is the first standby source. The later mappings of the same destination follow in mapping order. The copies of one source come in UID order;
     - under the one-copy rule, `maybeDiff` makes the copies that the rule cut from the folder of the plan into standbys of the planned copy. `mergeStandbys` puts them before the standbys of later mappings. A plan that copies by count gets no standbys from its own folder;
     - the standby stream from the folder of the plan sends no second EXAMINE, because `selectIfNeeded` finds the folder selected;
     - with `--verbose`, it logs one line when a standby copy replaces a planned UID. The line starts with `Copied message %s to %s from %s:` and a space. The three `%s` are the Message-Id, the destination folder and the folder of the standby copy. The program escapes all three. The end of the line gives the cause of the wait. Its `%s` is the escaped source folder of the plan. The three endings are:
       - refused body (`AppendMessage` refused the planned body as empty or missing): `the copy in %s has no usable body`;
       - not delivered (the stream did not deliver the planned UID, and the check counts it as still held): `%s did not deliver the planned copy`;
       - gone (the check found that the source no longer holds the planned UID): `%s no longer holds the planned copy`;
     - a waiting UID that no standby replaces counts as one error, or as gone when the check found that the source no longer holds it. Without standbys, the refusal counts as one error;
     - one standby stream requests every standby UID of its source folder that belongs to a waiting planned UID (`standbyRequest`). The stream downloads these bodies in batches of `uidFetchBatchSize`, also after the first usable copy replaced the planned UID. The callback appends a copy only while its planned UID waits in `pending`, so the destination gets no duplicate. Only the one-copy rule gives one planned UID two or more standbys in one source, because `maybeDiff` adds the cut copies. Under the count rule, `reservationOwners` pairs one to one, so a source gives each planned UID at most one standby. The program accepts the extra download. It occurs only when the worker cannot copy a planned copy. The cause is a refused body, a message that the source does not deliver, or a message that the source no longer holds. It adds no APPEND, and it uses the shared read limiter (`--bps-down`);
     - the preview, the totals and the trackers do not change, and the destination gets no more copies than the sync plan shows.
   - A throttle stop ends the copy of the whole run. A throttled reply is a reply for which `client.IsThrottled` is true. `classifyError` stays the only code that reads reply text.
     - These events record a stop in the `throttleStop` of the run:
       - an APPEND, also a standby APPEND, that gets a throttled reply;
       - an APPEND or an `EnsureSession` call that returns a throttled give-up;
       - a source stream that a throttled reply ends. The EXAMINE or the UID FETCH gets the reply, and `streamFailure` reads it from `StreamError.Stop`. A throttled status of a UID FETCH records the stop also when every body arrived. Then the run counts no error for these messages, but it prints the stop line and ends with exit code 1;
       - the existence check (`ExistingUIDs`) that gets a throttled reply;
       - a CREATE, or a read of the check after CREATE, that gets a throttled reply (step 4).
     - The first reply wins. The stop holds the side (`source` or `destination`) and the error.
     - The plan that sees the stop ends. It sends no UID SEARCH and starts no standby stream. Each planned UID that it did not copy counts as an error, also a UID that the source no longer holds. The exception is a UID with no standby that the check found gone before the stop. It stays gone.
     - The other running plans read the stop before each APPEND and before each batch. The halt hook of `StreamMessagesByUIDs` reads it before a batch. They end in the same way.
     - The dispatch loop starts no plan after the stop. A worker that takes a plan after the stop sends no command. Each plan that did not start counts all its messages as errors, and its tracker shows `Stopped`.
     - `runSync` prints this line one time, also with `--quiet`: `⛔ Sync stopped: the %s server [%s] reports a rate limit or a full quota: %s. Run sync again later: it continues where this run stopped.` The first `%s` is `source` or `destination`. The second is the label of the side. The third is the escaped error text. The line comes after the folder-failure list and before the error summary line.
     - The run ends with exit code 1 (`ErrSilentExit`). It does not wait, it does not cancel the context, and it does not print `Cancelled.`
     - To end a batch in flight, `StreamMessagesByUIDs` closes the source session (Section 3). The worker does not download the rest of the batch.
     - The next run continues through the Message-Id diff.
   - A destination that gave up does not make the worker download bodies that it cannot append:
     - `runFolderSync` calls `EnsureSession` on the destination Client before it reads the source. It calls it again after each failed APPEND that is not a refused body;
     - `EnsureSession` sends no command on a working session. While the give-up and revive gate refuses, it returns the stored give-up with no dial. After `reviveInterval`, it makes the one revive login, and the worker downloads no body for it;
     - if the call fails, the plan ends. The rest of its messages count as errors, and no standby search runs. A throttled give-up also records a stop;
     - a give-up that is not throttled records no stop. The worker takes the next plan, and `EnsureSession` fails again with no download while the gate refuses;
     - an APPEND that a working session refuses, for example with `NO [CANNOT] ...`, counts one error. The plan goes on.

Idempotency comes from the diff for each Message-Id: the count diff, or the one-copy rule for a destination that advertises X-GM-EXT-1. There is no UID-based bookkeeping; re-running on a partially completed sync is safe and resumes. The UIDVALIDITY check compares the sync plan of one run with the server and keeps no record between runs.

**Folder names in the mailbox cache** (`internal/client/cache.go`):

- A flat server has no hierarchy delimiter. When no LIST entry names a delimiter, `GetDelimiter` returns an empty string. This happens when the entries answer NIL and when LIST has no entry. The code does not use `/` in its place. The cache delimiter is the first delimiter that a LIST entry names. An entry with NIL does not change it.
- With an empty delimiter, `listSubfoldersFromCache` returns no child. `CreateMailbox` sends one CREATE for the whole name and creates no parent. Only a bare `INBOX` folds.
- The cache keeps each name as LIST gave it, after the decoding of modified UTF-7. go-imap folds a bare INBOX to upper case.
- RFC 3501 makes INBOX case-insensitive. `canonicalName(name, delimiter)` applies the same rule to a leading INBOX level and folds it to upper case. The level is the bare name, or the name followed by the cache delimiter. A name `Inbox/Sub` folds when the cache delimiter is `/`. It does not fold when the cache delimiter is `.`.
- `inboxSpellings` maps a folded name to its listed spelling, only where the two are different. If one folded name has several listed spellings, the smallest in byte order wins. So the answer does not depend on the LIST order.
- `resolve` finds the listed name in this order: the exact name, the folded name, the indexed spelling. A miss returns the folded name. `MailboxExists`, `IsNoSelect` and `CreateMailbox` use it. `ListSubfolders` and `folderLock` use the folded name. `MailboxNames` returns the listed spellings in byte order. `printCaseVariantDestinations` relies on this order.
- `runSync` resolves typed names and derived destination names with `ResolveName`, so EXAMINE, STATUS, CREATE and APPEND carry the listed spelling. A name that the cache does not list gets the folded INBOX level and no other change.
- go-imap v1 aborts the whole LIST when one entry has a name that is not valid modified UTF-7. `listAll` runs LIST with its own handler (`listHandler`). The handler leaves out such a name and records the raw name. Any other parse error still fails the LIST. The callback runs on the go-imap reader goroutine, so it must not block or call the client.
- `InvalidMailboxNames` returns the sorted raw names of the latest LIST, each name once. No mapping, plan or command holds them. The raw names are for printing and for the check in step 4.
- `loadMailboxCache` and `ListMailboxes` publish the result of a LIST only after the attempt succeeds. After a failed command write, go-imap can call the callback after `Execute` returns. So a retry keeps no name, no delimiter and no left-out name of a failed attempt.

### 3. Connection resilience (`internal/client/client.go`, `internal/client/watchdog.go`)

- `safeCall(fn)` wraps every IMAP op. Errors are routed through `classifyError`: only `ClassTransient` triggers a reconnect-and-retry; `ClassThrottled` and `ClassPermanent` are surfaced to the caller. Add new IMAP operations through `safeCall` rather than calling `c.Client` methods directly. `safeCall` runs each attempt of `fn` through `call`, which also opens one watchdog span. In the copy phase, the worker does not retry a throttled reply. The worker asks `client.IsThrottled`, and a throttled reply stops the run (Section 2, step 5).
- `classifyError` gives the class of an error. `io.EOF`, `net.ErrClosed`, each `net.Error` and `imapclient.ErrNoMailboxSelected` are `ClassTransient`. When a BYE ends the session after the EXAMINE, go-imap returns `ErrNoMailboxSelected` and does not send the command. A reconnect gives a new session, and the retry selects the folder again. For all other errors, `classifyLeaves` reads the text.
- `classifyLeaves` calls `errors.Unwrap` until it gets the leaf error and matches text on that leaf only. For an error that wraps a list, it classifies each item in order and returns the first class that is not `ClassUnknown`. So text that our wrappers add, such as a folder name or the `[label]` prefix, never sets the class. A folder name that contains `exceeded quota` cannot change the class of a dropped connection to `ClassThrottled`. Wrap an error with `%w`. A `%v` makes a new leaf that holds the wrapper text. A server reply can repeat a folder name. Then the name is part of the leaf text. See the Known limitation "a refusal that repeats a folder name" at the end of this section.
- `classifyMessage` matches phrases in the text of the leaf error, in this order. The first rule that matches decides:
  1. the locked-account phrases (see the locked-account item below): `ClassPermanent`;
  2. `too many simultaneous`, `session expired`, or `exceeded` with `bandwidth`, `limit` or `quota`: `ClassThrottled`;
  3. `not logged in` or `imap: connection closed`: `ClassTransient`;
  4. `temporary authentication failure`, or `authentication` with `temporarily unavailable`: `ClassTransient`;
  5. `authentication` with `fail`, `invalid credentials` or `no such mailbox`: `ClassPermanent`;
  6. all other text: `ClassUnknown`.
- Rule 4 matches an outage of the authentication service, for example the Dovecot reply `[UNAVAILABLE] Temporary authentication failure`. It comes before rule 5, so `reconnect` retries a LOGIN that gets this reply with backoff, and does not give up at once. go-imap v1 drops RFC 5530 response codes such as `[UNAVAILABLE]` from its errors, so the text decides.
- `reconnect` enforces a minimum interval (`reconnectInterval = 10s`), exponential backoff up to `maxReconnectAttempts = 5`, and a longer cool-down (`throttledBackoff = 5m`) when the previous attempt got `ClassThrottled`. `reconnect` waits only between two attempts. When the last attempt fails, `reconnect` returns the error immediately.
- The give-up and revive gate limits the login attempts of a Client after a failed `reconnect`. A login attempt is one dial and one LOGIN or AUTHENTICATE. When `reconnect` gives up, it stores the error in `giveUpErr` and stamps `lastReconnect`. This stored error is the give-up. A cancelled `reconnect` stores nothing, and a cancelled login attempt keeps the earlier give-up.
- The give-up and revive gate runs in `reconnect` under `c.mu`, after the cancellation check. So after `Cancel()`, `reconnect` returns `context.Canceled`, not the stored give-up. The gate runs before the `reconnectInterval` wait, the logout of the old session and any dial.
- `canRevive` reads the class of the stored give-up, and the class decides what the give-up and revive gate does:
  - `ClassPermanent` or `ClassUnknown`: the give-up is final. Each later call returns the stored error at once and does not dial, until the run ends. `ClassUnknown` is final because some servers refuse wrong credentials with text that no rule matches.
  - `ClassTransient` or `ClassThrottled`: calls return the stored error until `reviveInterval = 5m` passes. Then one call makes one login attempt, with no backoff and no throttle wait.
    - If that login attempt succeeds, `giveUpErr` becomes nil. The next `reconnect` then has all `maxReconnectAttempts` and the initial backoff again.
    - If that login attempt fails, `reconnect` stamps `lastReconnect` again, so `reviveInterval` starts again. A permanent or unknown failure makes the give-up final.
- While the give-up and revive gate holds a give-up, each Client makes at most one login attempt per `reviveInterval`. Clients do not share the gate. During `sync`, one side has one planning Client and one Client for each worker. Workers are at most 10, so one side has at most 11 Clients. The planning Client logs out before the workers connect, so at most 10 Clients of one side hold a session at the same time.
- Each Client stores the interval as `reviveDur`, and `New` sets it. A zero value turns the wait off (test fixtures only), like `logoutDur`. No config field or CLI flag sets it.
- `safeCall`, `AppendMessage` and `EnsureSession` call `reconnect` when the Client holds no session. `AppendMessage` and `EnsureSession` also call it when `sessionEnded` returns true. Only `New` and `reconnect` call `connectAndLogin`, so every login after `New` goes through the give-up and revive gate. A new caller of `connectAndLogin` bypasses the gate.
- `safeCall` starts at most one `reconnect` for each call:
  - When the Client holds no session, `safeCall` calls `reconnect` first. Then it runs `fn` one time and returns the error of `fn`, also when the error is transient.
  - After a transient error of `fn`, `safeCall` calls `reconnect` and runs `fn` one more time.
- `safeCall`, `AppendMessage` and `EnsureSession` get a new session from `reconnectSession`, which calls `reconnect`. When `reconnect` returns an error, `reconnectSession` returns that error. When `reconnect` returns nil, `reconnectSession` checks again whether `Cancel()` ran. If `Cancel()` ran, it returns `context.Canceled`. If the Client holds no session, it returns an error. In each of these three cases, `safeCall` does not run `fn` after the reconnect, and `AppendMessage` does not send the APPEND.
- `EnsureSession(ctx)` tells the worker whether the destination Client can still take messages. It runs `defer c.withCancel(ctx)()`. It returns the context error, or `context.Canceled` after `Cancel()`, before it does anything else.
  - A Client with a working session gets no command and no lock. `EnsureSession` returns nil.
  - If the Client holds no session, or `sessionEnded` returns true, `EnsureSession` calls `reconnectSession`. So the give-up and revive gate decides: a refused gate returns the stored give-up with no dial. After `reviveInterval`, one login attempt runs. A session that ended while idle gets a normal reconnect.
  - When the call fails and the context ended, it returns the context error. If not, it returns the error of `reconnectSession`. The class of that error tells a throttled give-up from a permanent one.
- A reply text that contains `locked`, `lockout`, `disabled`, `suspended` or `blocked` is `ClassPermanent`. This rule comes before the throttle and outage rules, because a login attempt on a locked account can make the lock longer. So a give-up on such a reply is final, and that Client makes no more login attempts.
- All `time.Sleep` calls in the reconnect path go through `sleepCtx`, which bails out on the internal cancel channel (closed by `Cancel()`) - Ctrl-C aborts immediately instead of waiting for the backoff to elapse.
- `Cancel` + `withCancel(ctx)` bridge `context.Context` to the underlying `*client.Client`: when the context is canceled, the connection is `Terminate()`d so blocked IMAP calls return immediately. `withCancel` registers `Cancel` through `context.AfterFunc`. After the returned stop function runs, a later cancellation does not change the client. Every long-running public method on `Client` should call `defer c.withCancel(ctx)()` and check `ctx.Err()` after each blocking step. `connectAndLogin` also does this and stores the unauthenticated `cli` in `c.c` before calling `Login` so `Cancel()` can `Terminate()` a stalled LOGIN.
- `connectAndLogin` checks for cancellation again after it stores the new session in `c.c`. `Cancel` sets the cancelled flag before it reads `c.c`, so one side always sees the other. Thus a `Cancel()` between the greeting and the store stops the login: `connectAndLogin` does not send LOGIN and returns `context.Canceled`. During the dial and the greeting, `ctx` ends the wait: the caller context in `New`, or the internal context that `Cancel()` ends in `reconnect`.
- `loadMailboxCache` (the first `LIST "" "*"` in `New`) runs under `withCancel(ctx)` and checks `ctx.Err()`, so Ctrl-C stops `New` during that LIST. The same holds for the CAPABILITY read that follows it. When a cancellation causes a failure, `connectAndLogin`, `loadMailboxCache` and `detectOneCopyPerID` return `ctx.Err()`, or `context.Canceled` after a `Cancel()`. `cmd/imapsync-go/main.go` then prints `Cancelled.` and does not print a connection error. It exits with code 128 plus the signal number: 130 after SIGINT (Ctrl-C) and 143 after SIGTERM. A cancel that no signal names exits with code 130.
- `New` runs `connectAndLogin`, then `loadMailboxCache`, then `detectOneCopyPerID`. The last step reads the capability that sets the copy rule of the sync plan (Section 2, step 3).
  - `detectOneCopyPerID` runs under `withCancel(ctx)` and checks `ctx.Err()` before the command and after it. It sends one CAPABILITY through `safeCall` after LOGIN, also when the LOGIN reply carried a capability list. go-imap sends one more CAPABILITY before LOGIN only when the greeting has no capability list (see `greet`).
  - `hasCapability` finds `X-GM-EXT-1` in any letter case and allocates nothing. go-imap keeps each capability name as the server spelled it, and its `Support` compares names exactly.
  - `New` sets the flag one time, before it returns, so `KeepsOneCopyPerMessageID` needs no lock. A reconnect does not read the capability again.
  - A transient failure gets one reconnect from `safeCall`, and the reply of the new session decides.
  - A refused CAPABILITY fails `New`. It logs out and returns an error that starts with `failed to read server capabilities:` and a space, and wraps `[label] capability: <reply>`. When the context ends, `detectOneCopyPerID` returns the context error, and `New` returns an error that wraps it.
  - Every `client.New` sends this CAPABILITY: the planning Clients, each worker Client and the `show` Clients. Only the flag of the planning destination Client changes the sync plan (`copyRuleOf(dstClient)` in `buildSyncPlan`). The source flag never changes the sync plan.
- TCP keepalive is enabled at the dialer (`tcpKeepAlivePeriod = 30s` idle interval, via `newDialer`) so half-open sockets after a Wi-Fi flap are detected within the OS-default keepalive envelope rather than hanging indefinitely.
- `connectAndLogin` calls `greet(ctx, conn)` in place of a bare `imapclient.New`. `greet` closes the connection when `ctx` ends, or when the greeting does not arrive within `dialTimeout` (default `defaultDialTimeout = 30s`). The limit also covers the CAPABILITY probe that go-imap sends when the greeting has no capability list. The limit counts no wait for tokens, because the greeting and the probe take their tokens with no wait (see the `guardConn` item). A timer closes the socket, not `conn.SetDeadline`, because that probe clears connection deadlines. The timeout error wraps `os.ErrDeadlineExceeded`, so `classifyError` returns `ClassTransient` and `reconnect` retries it. `greet` wraps all other failures with the prefix `imap greeting:`.
- `logout(cli)` sends LOGOUT and waits a maximum of `logoutTimeout = 2s`. Each Client stores this limit as `logoutDur`, and a zero value means no limit (test fixtures only). If the server does not answer, `logout` terminates the connection and returns an error that wraps `errLogoutTimeout`. `logout` always closes the socket before it returns, because go-imap closes the socket only when the server sends BYE. `logout` does not use `safeCall`, so a logout never triggers a reconnect. `Logout()`, `reconnect` (when it discards the old session under `c.mu`) and the cleanup after a failed LOGIN or AUTHENTICATE call `logout`.
- `idleConn` (`internal/client/watchdog.go`) is the inactivity watchdog for commands in flight. `idleTimeout = 5 * time.Minute` is its limit. Each Client stores the limit as `idleDur`, and `New` sets it. `New` uses `Options.IdleTimeout` when this value is more than zero and not more than `idleTimeout`. In all other cases, `New` uses `idleTimeout`. So the field can only make the limit shorter. Only tests set `Options.IdleTimeout`. No config field, CLI flag or environment variable sets it. A zero `idleDur` turns the watchdog off (test fixtures only), like `logoutDur`. `New` never sets a zero value.
- `connectAndLogin` calls `guardConn` after `dialFn` and before `greet`. `guardConn` wraps the dialed transport in `idleConn`. When a limiter is set, it then wraps the result in `ratelimit.Conn`. So the watchdog is the innermost wrapper, and a wait for rate-limit tokens does not run inside it. `dialFn` only dials TCP or TLS.
  - `guardConn` returns the `ratelimit.Conn` as a third result. The result is `nil` when the Client has no limiter. `guardConn` calls `SetWait(false)` on the wrapper before it returns.
  - So the greeting and the CAPABILITY probe of go-imap take their tokens from the shared limiter with no wait. `imapclient.New` returns only after it reads the probe reply, so the mode with no wait covers all greeting traffic.
  - When `greet` returns a client, `connectAndLogin` calls `SetWait(true)`. This call comes before the first LOGIN or AUTHENTICATE byte, for `New` and for each reconnect. LOGIN and all later traffic wait for tokens.
  - The reason: during the copy, the other Clients keep the shared limiter in debt. If the greeting waits for this debt, the wait counts against `dialTimeout`. Then each reconnect fails at a low `--bps-up` or `--bps-down`, and the worker gives up.
  - A `Read` that began before `SetWait(true)` waits, because `Read` loads the mode after the inner read returns. So the reply to LOGIN waits for tokens.
- `c.idle` holds the watchdog of the session in `c.c`. `connectAndLogin` stores `c.idle` before `c.c`, so a reader that sees the new session also sees its watchdog. `call` loads `c.idle` again on each attempt, so a retry after a reconnect uses the new watchdog.
- A span is the time while a command is in flight. The watchdog counts only time inside a span. `safeCall` runs each attempt of `fn` through `call`, which opens one span. `AppendMessage` runs each `cli.Append` attempt through `call`, which opens one span. `connectAndLogin` opens one span around LOGIN or AUTHENTICATE, for the first connect and for each reconnect. A go-imap call outside `safeCall` must open its own span.
- The watchdog does not cover the greeting or LOGOUT. The greeting keeps its `dialTimeout` limit, and LOGOUT keeps `logoutTimeout`.
- The watchdog closes the transport when all these conditions are true:
  - a span is open;
  - at least one `Read` or `Write` is blocked on the transport;
  - no byte moved in either direction for `idleDur`.
- Any byte in either direction resets the idle time to zero. The watchdog limits inactivity, not total time, so a transfer that moves bytes is never cut. A session that is idle between commands is never closed.
- When the watchdog trips, the pending call fails. `idleErr` wraps its error with `errIdleTimeout` (`no data from server`) and `os.ErrDeadlineExceeded`, so `classifyError` returns `ClassTransient`. `safeCall` then reconnects and retries once, inside the `reconnectInterval`, `maxReconnectAttempts` and backoff limits. Example error text: `imap: connection closed: no data from server within 5m0s: i/o timeout`. Before the reconnect, `noteIdleTrip` writes one line with the reason to the progress writer, or to the tracker when no writer is set. It does this also without `--verbose`, because a trip is rare.
- A LOGIN that gets no reply during a reconnect trips too, so `reconnect` never holds `c.mu` without a limit. A trip during an APPEND follows the `AppendMessage` rules in Section 5.
- A trip never calls `Cancel` and never sets the cancelled flag. Each cancellation check runs before the error handling. So Ctrl-C returns `context.Canceled`, also after a trip.
- `StreamMessagesByUIDs` ends the span while `onMessage` runs, because that time is our own work (usually the destination APPEND). At the end of a batch the source is done and silent. Code that runs caller work inside a `safeCall` closure must also end the span, and open it again after. A wait for read tokens runs outside `idleConn`, so it never counts.
- Accepted edge: the watchdog cannot see a wait for write tokens. If one literal write (up to 32 KiB) waits `idleTimeout` or more for tokens, the watchdog trips. This needs an upload limit below about 110 bytes per second for each worker that uploads at the same time. The limiter is shared between the workers. `runSync` prints a warning before the plan when `--bps-up` is below `client.MinUploadBPS(workers)`, which computes that floor from `maxLiteralWrite` and `idleTimeout`.
- Each connection has one timer, and `Close` stops it. Without an open span the timer does not run. A timer that is pending when a span ends fires once, finds no span and stops.
- `selectIfNeeded(cli, folder)` sends no EXAMINE only when `cli.State()` is `imap.SelectedState` and `cli.Mailbox()` has the name `folder`. go-imap clears the selected mailbox after a BYE and after a failed EXAMINE. A new session after a reconnect has no selected mailbox. The Client does not store a copy of the selected folder, because a copy can be different from the session. **Use it instead of `cli.Select` directly** - `StreamMessagesByUIDs` saves one round-trip per 500-message batch this way.
- `StreamMessagesByUIDs` batches `UID FETCH` in chunks of `uidFetchBatchSize = 500` to avoid IMAP "Too long argument" errors. Don't unbatch.
- `StreamMessagesByUIDs` sends the UID FETCH through `uidFetchBodies`, which uses `bodyFetchHandler` in place of the `UidFetch` of go-imap. The items are `ENVELOPE`, `FLAGS`, `INTERNALDATE` and `BODY.PEEK[]`.
  - `bodyFetchHandler` keeps the flag spelling of the server, because go-imap lower-cases keywords and the copy must keep the spelling of the source.
  - It treats a response with no UID, or with a UID that the stream did not ask for, as unilateral data. It refuses a response that has a message number and no item list (`errFetchNotEnoughFields`). Only that batch fails.
  - The message channel has no buffer. go-imap holds each body fully in memory, so each slot of a buffer keeps one more body. With no buffer, go-imap reads one body while `onMessage` holds another. So a worker holds at most two bodies.
  - `messageChanBuffer` is only for the scans that fetch no body.
- Both scans (`FetchMessageMap` and `FetchMessageCopies`) size their map with `min(total, maxMessageMapHint)`, and `maxMessageMapHint = 1 << 16`. The server sets `total`, from EXAMINE or from STATUS MESSAGES. A very large count must not cause a large allocation. The map size still increases to the real number of messages.
- A retry of a `StreamMessagesByUIDs` batch (after a transient error, through `safeCall`) fetches only the UIDs whose body `onMessage` did not receive. If `onMessage` received all of them, the retry sends no EXAMINE or FETCH command. The code does not deliver a second response with the body for the same UID. So a retry creates no duplicate APPEND on the destination. A response without a body never reaches `onMessage` and does not mark its UID, so the full response that follows can still deliver the message.
- An error of `onMessage` or of `halt` ends the stream, and `StreamMessagesByUIDs` returns it unchanged.
  - The closure returns `errStreamStopped`, an error with no class. So `safeCall` starts no reconnect and no retry, also when the batch error is transient.
  - If at least one UID of the batch is not yet delivered, the stream calls `Terminate` on the session. go-imap v1 cannot end a FETCH in flight in another way. Without the close, the stream downloads every body that remains in the batch. The stream then reads the rest of the responses with no callback until the reader ends.
  - The Client keeps the closed session in `c.c`. The next command on this Client fails as transient, and `safeCall` reconnects through the normal path.
- `StreamMessagesByUIDs(ctx, folder, uidValidity, uids, halt, onMessage)` checks the UIDVALIDITY on every attempt of every batch, also a retry on a new session.
  - `halt` is a function that returns an error or nil. A nil `halt` skips the check. The stream calls `halt` at the top of each batch and at the top of each attempt, before any command. An error of `halt` ends the stream and goes back unchanged. So a retry on a new session sends no UID FETCH after a stop.
  - After `selectIfNeeded`, it calls `CheckUIDValidity` with the value of the session. This value is the EXAMINE result, or `cli.Mailbox()` when no EXAMINE ran.
  - If the values are different, it sends no UID FETCH and returns the error. A `uidValidity` of 0 skips the check. Batches that the stream already delivered stay delivered.
  - The text of `ErrUIDValidityChanged` is `source folder changed after the sync plan, run sync again`. No `classifyMessage` rule matches it, so its class is `ClassUnknown`, and `safeCall` does not reconnect for it.
- Folder creation uses a per-path `sync.Mutex` (`folderLocks` map) to make concurrent `CreateMailbox` calls for nested paths race-free; parents are created walking down the hierarchy.
- `CreateMailbox` and `createParentFolders` treat a CREATE refusal as success when `isAlreadyExistsErr` matches the error text: `already exists`, `mailbox exists` or `duplicate folder name` (the Gmail text). go-imap v1 drops the `[ALREADYEXISTS]` code, so the text decides. They give `isAlreadyExistsErr` the error from `safeCall`, before a wrapper adds a folder name. They add the mailbox to the cache.
- `c.pw` and `c.tracker` are `atomic.Pointer[progressWriterRef|progressTrackerRef]`. Read via `c.progressWriter()` / `c.progressTracker()`; do not access the fields directly.
- `Client.log` sets the text of the tracker when `c.tracker` is set. If no tracker is set, it writes to the progress writer, and only with `--verbose`. `noteIdleTrip` and the missing-Message-Id warning write to the writer, also without `--verbose`.
- From the scan to the logout before the worker pool, each planning Client has a writer. Each worker Client gets the copy writer after `newSyncWorkerPool` returns, so a watchdog trip during its `New` prints no line. With `--quiet`, the writers print nothing. The scan sets the writer and the tracker directly. The other phases use `routeClientLogs`, which sets the writer and removes the tracker. The sinks of each phase are:
  - scan: the scan writer, and the scan tracker of the side;
  - after the scan and after the creation: `lineLog` and no tracker. `lineLog` prints each line at once to `out`, and with `--quiet` it prints nothing. go-pretty drops a log while no tracker is active, and while a tracker is set, `Client.log` puts the line into the tracker text;
  - creation: the creation writer, for the destination planning Client;
  - copy: the copy writer, for both Clients of each worker.
- go-imap writes every read error to stderr, including the error that each `Terminate` causes. `greet` calls `routeErrorLog` right after `imapclient.New` returns, also for a client that go-imap returns with an error. Without `--verbose`, the log goes to `io.Discard`. With `--verbose`, the logger output goes through `termsafe.NewWriter`, because the lines quote raw server responses. The function changes the output of the existing `*log.Logger` with `SetOutput`, because the reader goroutine already runs and a field write races with it. Section 6 describes the lines that go-imap logs before `routeErrorLog` runs.

**Known limitation - Ctrl+C blocked inside a literal write.** go-imap sends a synchronizing literal in two steps. First the client sends `{N}\r\n`. Then it waits on `<-w.continues` (`vendor/github.com/emersion/go-imap/write.go:158`) for the `+` reply of the server. Only the response handlers of the reader goroutine send on that channel.

If the socket closes before the server sends `+` or a tagged reply, the reader goroutine ends and the wait never ends. These events close the socket: `Cancel()` / `Terminate()`, TCP keepalive expiry (`tcpKeepAlivePeriod = 30s` idle interval, several minutes on macOS and Linux) and a watchdog trip. The goroutine that writes the literal stays blocked until the process exits.

The watchdog does not change this limitation. It closes the socket in this window, but a closed socket does not release the wait. Only the worker inside the in-flight operation is affected, and other workers continue.

Users can press Ctrl+\ (SIGQUIT) to get an immediate goroutine dump and exit. A fix needs one of two changes. The first patches `writeLiteral` in the vendored go-imap, to `select` on a done channel with the `continues` receive. The second moves to go-imap v2.

**Known limitation - a refusal before the continuation request.** When the destination refuses an APPEND with a tagged NO before it sends `+`, go-imap v1 returns only `imap: cannot send literal: no continuation request received`. It drops the reply of the server. This error has the class `ClassUnknown`. So the refusal counts as one refused APPEND, and the run goes on. The run does not stop for a throttled reply that comes in this form. A fix needs go-imap v2.

**Known limitation - a refusal that repeats a folder name.** `classifyError` reads the reply text of the server. Some servers repeat the folder name in a refusal, for example the Dovecot reply `Mailbox doesn't exist: NAME`. Then NAME is part of the leaf text, and a phrase in NAME can set the class of the refusal.

- If NAME holds a throttle phrase, for example `Quota exceeded`, the refusal has the class `ClassThrottled`. Then the whole run stops with the `Sync stopped` line (Section 2, step 5). The line shows the reply, so the operator can see the cause. This holds for a refusal in the CREATE step, in the check after CREATE and in the copy.
- A person or a program can delete or rename a folder during the run. Then the EXAMINE of the source folder or the APPEND to the destination folder can get such a refusal.
- An APPEND refusal that comes before the continuation request has no reply text (see the Known limitation above). So NAME has no effect on that refusal.
- In the planning scan, `FetchMessageMap` does not mark a throttled or transient refusal with `ErrSelectRefused`. So an unlisted parent with such a name is a folder failure, not a skip (Section 2, step 3).
- If NAME holds a transient phrase, for example `Not logged in`, the refusal has the class `ClassTransient`. In `FetchMessageMap` or `FetchMessageCopies`, it costs one reconnect. `Test_FetchMessageMap_When_RefusalEchoesTransientPhraseOfFolderName_Then_ReconnectsOnce` checks this for `FetchMessageMap`.
- In the copy, each batch of `StreamMessagesByUIDs` sends its own EXAMINE. So a transient refusal costs one reconnect for each batch of `uidFetchBatchSize` UIDs, and `ExistingUIDs` costs one more.
- An APPEND refusal can come after go-imap sends the body. go-imap sends the body with no wait when the body has at most 4096 bytes and the server advertises LITERAL+ or LITERAL-. If NAME holds a transient phrase, `AppendMessage` repairs the session after such a refusal (Section 5). The worker then goes on with the next message, so each message costs one reconnect.
- `reconnectInterval` keeps the reconnects of one Client at least 10 seconds apart.
- The run loses no mail. The next run makes a new plan from the current folders.
- go-imap v1 keeps the response code only in the status response. `Select`, `Create` and `Append` return `status.Err()`, which holds only the text. A full fix needs go-imap v2, which gives the codes `[NONEXISTENT]` or `[TRYCREATE]` and `[OVERQUOTA]` or `[LIMIT]`.

**Known limitation - a refusal that arrives with a closed socket.** A server can send a tagged NO or BAD and close the socket at once. Then go-imap v1 can return the closed session in place of the reply.

- The `select` in `execute` (`vendor/github.com/emersion/go-imap/client/client.go:276-292`) has a case for `c.loggedOut` and a case for `doneHandle`. `c.loggedOut` closes when the reader goroutine ends. `doneHandle` carries the status reply. When both are ready, Go picks one at random.
- If `c.loggedOut` wins, the refusal comes back as `imap: connection closed`. This error has the class `ClassTransient`.
- For LOGIN inside `reconnect`, the class decides the number of attempts. With the right class, a `ClassPermanent` refusal ends `reconnect` after one LOGIN. Examples are a locked account and the text `Authentication failed`. A throttled reply waits `throttledBackoff = 5m` before the next attempt.
- With the wrong class, `reconnect` retries with backoff, up to `maxReconnectAttempts` LOGIN attempts. The give-up that follows is `ClassTransient`, so it can revive after `reviveInterval` (this section). A `ClassUnknown` refusal gets up to `maxReconnectAttempts` LOGIN attempts also with the right class. For such a refusal, the wrong class only lets the give-up revive.
- Other commands have the same race. A throttled reply to EXAMINE, UID FETCH, CREATE or APPEND can come back as `imap: connection closed`. Then the run records no throttle stop for that reply, and the client reconnects as for a dropped connection (this section and Section 5).
- The case needs the goroutine of `execute` to wait for a CPU until the reader goroutine ends. So it is rare.
- A fix needs go-imap v2.

### 4. Rate limiting (`internal/ratelimit`)

- `ratelimit.NewLimiter(bps)` returns `nil` when `bps <= 0` - the caller's nil-check is the "unlimited" signal.
- `ratelimit.New(net.Conn, read, write *rate.Limiter)` wraps a connection so reads and writes block until tokens are available. `guardConn` installs the wrapper in `connectAndLogin`, after `dialFn` returns and outside `idleConn`. So go-imap is unaware of throttling, and the watchdog never counts a wait for read tokens (see Section 3).
  - A new wrapper waits for tokens. `SetWait(false)` makes each `Read` and `Write` take its tokens at once, with no wait. `SetWait(true)` turns the waits on again. Both calls are safe while a `Read` or a `Write` runs.
  - With no wait, `chargeN` reserves the tokens in chunks of at most the burst of the limiter, because `ReserveN` charges nothing for a larger request. It does nothing when the burst is 0. The debt delays only a later caller that waits.
  - `guardConn` turns the waits off for the greeting. `connectAndLogin` turns them on before LOGIN (see Section 3).
  - `Close` ends a wait in progress in both modes.
- One limiter per direction is shared across every `Client` that talks to the same side (one `srcReadLim` for all source workers, one `dstWriteLim` for all destination workers). That makes the BPS budget a global cap, not per-connection.
  - The greeting and the CAPABILITY probe of each Client also draw from this budget, with no wait. So the budget stays one global cap for each direction.
  - The debt of a greeting delays the next LOGIN or transfer of each Client that shares the limiter.
  - Only the server of that side makes the greeting bytes large. That server is the peer of each Client that shares the limiter.

### 5. Body fetch semantics

`StreamMessagesByUIDs` fetches via `BODY.PEEK[]` (`fullBodyPeekSection`), not `FetchRFC822`. The two are functionally equivalent for transferring the message but `FetchRFC822` sets `\Seen` on every message - a state mutation a sync tool must not introduce on the source. If you add new fetch sites, mirror this choice.

`AppendMessage` passes `msg.GetBody(...)` straight to `cli.Append` - no `io.ReadAll` round-trip - to halve peak RAM with large attachments. go-imap reads the body one time.

`StreamMessagesByUIDs` fetches `FLAGS` and `INTERNALDATE` with the body, and `AppendMessage` sends both with the APPEND. The flags and the date come from the message that `AppendMessage` receives. So a standby copy carries its own flags and date.

- `appendFlags` picks the flags. It keeps each keyword, and the system flags `\Seen`, `\Answered`, `\Flagged` and `\Draft`. It drops `\Recent`, because APPEND cannot set it. It drops `\Deleted`, because an EXPUNGE removes each message with this flag. It drops each other system flag, because a destination can refuse an unknown system flag on every run.
- `validFlag` drops each flag that is not an atom, because go-imap writes a flag raw into the command line. A flag with a space, a control byte, a byte above 0x7e, a quote, `(`, `)`, `{`, `%`, `*` or `]` is not an atom. The flag cannot hold a backslash after the first character. Such a flag can add a command to the destination session.
- If no flag stays, the APPEND has no flag list. A source message without `\Seen` is unread on the destination.
- `appendDate` sends the INTERNALDATE of the source. If the source sent none, it sends the `Date` header of the envelope. If that is also missing, the APPEND has no date and the destination stamps its own.
- The program copies the flags and the date one time, when it uploads the message. The program does not copy a later change of the flags on the source, because the Message-Id diff finds no missing message.

`AppendMessage` refuses some messages before it reconnects, logs in or sends an APPEND. A message with no `BODY[]` section gives `ErrMissingBody` (`message has no body`). A body of length 0 gives `ErrEmptyBody` (`source returned an empty body`). go-imap gives an empty body for `BODY[]` NIL, for `{0}` and for a quoted string. An empty message has no Message-Id, so the diff of a later run cannot find it on the destination. Without the refusal, each run adds one more empty message. Section 2, step 5, describes how `runFolderSync` handles the refusal. Without a standby copy, the message fails again on each run until the source sends a body. `StreamMessagesByUIDs` gives the callback only a response with a body, so in the copy the refusal is `ErrEmptyBody`. A message for which the source never sends a body is a message that the source did not deliver (Section 2, step 5).

Before the APPEND, `AppendMessage` calls `reconnectSession` in these cases:

- the Client holds no session;
- `sessionEnded` reports that `cli.LoggedOut()` is closed, after a BYE or a closed socket.

After a BYE, go-imap refuses the APPEND with `Not logged in`. After a closed socket without a BYE, an APPEND can wait for the `+` continuation with no time limit (see the Known limitation in Section 3).

`readTracker` records the first `Read` call of go-imap on the body. The APPEND gets one retry when all these conditions are true:

- the error is `ClassTransient`;
- go-imap did not start to read the body, so the server did not get the message;
- this call did not reconnect before;
- the context did not end, and `Cancel()` did not run.

For the retry, `AppendMessage` calls `reconnectSession` one time and sends the APPEND again.

After go-imap started to read the body, `AppendMessage` does not send the APPEND again. The server can already have the message, and a second APPEND can make a duplicate. If the context ended, `AppendMessage` returns the context error and does not repair the session. If not, it calls `reconnect` to repair the session when the error is `ClassTransient`, this call did not reconnect before, and `Cancel()` did not run. Then it returns the APPEND error. The next sync run copies the message through the Message-Id diff.

One `AppendMessage` call reconnects a maximum of one time. `AppendMessage` does not use `safeCall`, because `safeCall` sends a command again after a transient error also when go-imap read the body. Each attempt runs through `call`, so each attempt has its own watchdog span.

### 6. Terminal output (`internal/termsafe`)

An IMAP server can send folder names, error text and Message-Ids that contain control, bidi or invisible characters. On a terminal, such text can move the cursor, clear the screen or make a false status line.

**Rule.** Show text from an IMAP server on the terminal only through `termsafe.Escape`. This covers:

- folder names (LIST, the mailbox cache, plans, and the mappings that come from them);
- reply text inside errors;
- envelope Message-Ids;
- go-imap error-log lines.

Text from the operator (labels, server, user, config path) is trusted, so print it as is. A new output site must follow this rule.

**Existing `%q` sites.** Some output sites in `internal/app/sync.go` quote server text with a `%q` verb. They stay as they are. A new output site must not use `%q` for server text. These sites are:

- the delimiter lines in verbose mode;
- the delimiter-mismatch lines and the "Fixed source" and "Fixed destination" lines;
- the "Failed to create folder" and "Created folder" logs;
- the "Mapping ... skipped" line;
- the "scan destination folder" error, which `reportError` also escapes.

`strconv.Quote` escapes each rune for which `strconv.IsPrint` is false. These runes are:

- all C0, DEL and C1 controls;
- all Cf runes, bidi controls too;
- `\u2028` and `\u2029`, private-use and unassigned code points;
- each invalid UTF-8 byte.

So `%q` is safe against control sequences.

`%q` keeps graphic but invisible runes, such as `\u3164`. Two different names can look the same in a `%q` line. `Escape` escapes these runes.

Do not replace `%q` with `Escape` at these sites: the output loses the escape of the quote characters. Do not put `Escape` inside `%q`: the output gets two layers of escapes. The verbose Message-Id line in `append.go` also uses `%q`. `Client.log` escapes the whole message, so `Escape` covers that line too.

**What `Escape` does.** `Escape(s)` keeps a rune only if `strconv.IsGraphic` is true and the rune is not in `unicode.Other_Default_Ignorable_Code_Point`. All other runes become a visible Go escape in lowercase hex: `\a \b \f \n \r \t \v`, `\xNN`, `\uNNNN` or `\UNNNNNNNN`. Each invalid UTF-8 byte also gets an escape.

`Escape` escapes these runes:

- C0 (tab, CR and LF too), DEL and C1;
- Cf runes: all bidi controls (U+202A to U+202E and U+2066 to U+2069), LRM, RLM, ALM, zero-width characters, BOM, soft hyphen and tags;
- U+2028 and U+2029, private-use and unassigned code points;
- graphic but invisible runes, such as U+3164 and U+034F.

`Escape` does not change letters of any script, punctuation, symbols, emoji, variation selectors (U+FE0F), spaces (U+00A0 too), backslash and quotes. If nothing needs escaping, it returns the input and does not allocate memory.

Backslash is not escaped, on purpose. A folder named `\x1b` looks the same as a folder that contains an ESC character. Both forms are inert text.

The guarantee holds for UTF-8 terminals. A terminal that acts on 8-bit C1 bytes inside multi-byte runes is out of scope.

`termsafe.Writer` (from `NewWriter`) escapes each `Write` and keeps one trailing newline raw, so each `log.Logger` line stays one line.

**Presentation boundary.** Escape only where the program prints the text. These values stay byte-exact:

- the arguments of SELECT, EXAMINE, STATUS, CREATE and APPEND, which go back to a server;
- plan data, mappings and map keys;
- the names that `MailboxNames` and `InvalidMailboxNames` return. The print sites escape them;
- error values. `classifyError` and `isAlreadyExistsErr` match on raw server text. The `Err` fields of `MailboxInfo`, `FolderFailure` and `FolderSkip` also hold raw server text, and the print sites escape them.

A tracker line carries our own SGR colour codes. Escape the server parts before you compose the line, never the whole line.

**Where the program escapes.**

- `Client.log` escapes the whole composed message for both sinks. No format of `log` carries colour codes, so every caller of `log` is covered.
- Two direct sinks escape on their own: the missing-Message-Id warning in `fetch.go` and the per-mailbox tracker line in `mailbox.go`.
- `lineLog` and the progress writer print a Client line unchanged. `Client.log` and the missing-Message-Id warning escape the server text before they call `Log`. The line of `noteIdleTrip` holds only the label and a duration, so it needs no escape.
- `internal/app/sync.go` escapes these sites:
  - the plan preview;
  - the folders-to-create list, with the parent levels (`printFoldersToCreate`);
  - the creation tracker;
  - the "already exists" log;
  - the error text of the "Failed to create folder" log (the folder name there stays `%q`);
  - the scan trackers;
  - the found-subfolder line;
  - the folder-failure list (`printFolderFailures`), which shows the source folder, the destination folder and the reason. `runSync` prints it after the scan, also with `--quiet`. Without `--quiet`, `runSync` prints it again after the copy;
  - the "Folder ... skipped" line of `printExpansion`, for each source name that the mailbox cache marks `\Noselect` or `\NonExistent`. Only the name is server text. `printExpansion` prints the line only with `--verbose` and without `--quiet`;
  - the skip line for an unlisted parent that refuses to open (`printFolderSkips`). It shows the name, a fixed reason and the error text with the server reply. `runSync` prints it also without `--verbose`, but only without `--quiet`;
  - the "already exists and holds" line and the "no longer holds" line of `recheckExistingDestinations`. Each line shows the destination folder and the source folder. `runSync` prints each line only without `--quiet`. The failures of the same check go through `printFolderFailures`;
  - the "is left out" line of `printInvalidNames`, which shows the raw name from LIST. `runSync` prints it also with `--quiet`;
  - the "is renamed to" line of `printFolderRenames`, which shows the source name, the destination name and the destination delimiter. `runSync` prints it also without `--verbose` and with `--quiet`;
  - the clash failures of `splitDestinationClashes`. They go through `printFolderFailures`, and the reason names another source folder;
  - the empty-level failures of `splitEmptyLevelDestinations`. They go through `printFolderFailures`, and the reason shows the destination name and the delimiter. The code builds the name from a source folder name;
  - the CREATE refusals of `splitRefusedDestinations`. They go through `printFolderFailures`, and the reason holds the reply of the server;
  - the stop line of `printThrottleStop`, which shows the escaped error text. The labels in the line are operator text. `runSync` prints it also with `--quiet`;
  - the case-variant warning of `printCaseVariantDestinations`, which shows the destination names. `runSync` prints it also without `--verbose` and with `--quiet`;
  - the "goes to ..., not to ..." line of `printExpansion`, which shows the source folder, two destination names and two root names. `printExpansion` prints it only with `--verbose` and without `--quiet`.
- `internal/app/show.go` escapes these sites:
  - the folder cell of each table row, also on the "unreadable" and "container" rows;
  - each line of the unreadable-folder list (`printUnreadableFolders`), which shows the folder name and the reason;
  - the "is left out" line of `printInvalidNames`. `runShow` prints it after the two tables, also with `--quiet`.
- `internal/app/worker.go` escapes every tracker line and log, also the plan rows of `newPlanTracker` and `newStoppedPlanTracker`, and the verbose Message-Id line. `trackerReason` escapes the inline error reason first, then cuts it to 40 bytes on a rune boundary.
- `reportError` in `cmd/imapsync-go/main.go` escapes the final `Error:` line, line breaks too. So a multi-line error prints on one line. Without the escape, a raw line break lets a server make a false line after `Error:`.
- `fetchTag` in `cmd/imapsync-go/helpers` escapes the GitHub release tag. Only `--version` and `-v` show this tag, for a build without `-X main.Version`. `printVersion` gets the tag through `LatestVersion`. The VERSION section of the root `--help` shows the text from `helpers.Version`. This function sends no request, so the help holds no text from GitHub.

**Known limitation - go-imap logs before `routeErrorLog` runs.** `routeErrorLog` (Section 3) covers the go-imap error log. But `imapclient.New` creates its logger on `os.Stderr`. It also reads the greeting and the implicit CAPABILITY probe before `routeErrorLog` can run. A line that go-imap logs in that window reaches stderr unescaped, in every mode.

In go-imap v1.2.1, such lines are parse errors and network errors. A parse error holds server text in only two places: one of the characters `(`, `{` or `"`, and a literal length that `strconv` quotes. Neither can carry a control sequence. Audit this again on each go-imap upgrade. A full fix needs go-imap v2, or a fork with a logger option. A patch of `vendor/` does not last, because `just deps` rewrites it.

## Conventions

- Configuration format is detected from the file extension: `.json` / `.yaml` / `.yml` (see `internal/config/config.New`). The CLI accepts either; sample files are `config.example.json` / `config.example.yaml`. The loader rejects a key that `Config` does not define. This applies at all levels, in both formats. JSON matches a key without regard to letter case, as `encoding/json` does, so `SRC` loads as `src`. YAML matches only the exact letter case. The error names the key and the file. The loader also rejects content after the first JSON value or YAML document. An empty or null YAML document after the first document has no settings, so the loader accepts it.
- `auth` accepts only an empty value, `login` or `cram-md5`, in any letter case. The loaded value keeps the letter case from the file. `config.New` rejects other values with `ErrAuthUnsupported`. The error names the block (`src` or `dst`) and the supported values. It never shows the value from the file, because the value can be a password in the wrong field. `client.New` also refuses an unknown `Options.Auth` before it dials. Its error also does not show the value.
- Workers are clamped to `[1, 10]` in `config.New`. A value out of range changes to the nearest limit.
- Optional `rate_limit` block in config (`down_bps`, `up_bps`, `max_connections`); CLI flags `--bps-down`, `--bps-up`, `--max-connections` override it. A flag that the user sets, or its environment variable, overrides the config value also when it is 0. For `--bps-down` and `--bps-up`, 0 removes the limit. For `--max-connections`, 0 uses the `--workers` value. The value caps the number of sessions that each side has at the same time, and `--max-connections=N` gives at most N workers. `config.New` tests `IsSet`, not a non-zero value.
- `config.New` rejects a `map` item with an empty or missing `src` or `dst` (`ErrMapSrcRequired`, `ErrMapDstRequired`). The error gives the position of the item, counted from 1. A `map` with no items is valid: `sync` then mirrors every source folder.
- `config.New` rejects two `map` items with the same raw `src` and different `dst` values (`ErrMapSrcConflict`: `one "src" folder has two different "dst" folders`). The error starts with `map entries J and I:`, where J and I are the positions of the two items, counted from 1. Two identical items load, and so do two items with the same `dst`. `config.New` compares the text only, so `runSync` makes the check again after `resolveMappings` (`checkSourceConflicts`).
- `ErrSilentExit` makes `reportError` return exit code 1 and print no `Error:` line. A command returns it after it prints its failures on stdout. `sync` uses it for failed messages and failed folders. `show` uses it for folders that it cannot read.
- `client.New(ctx, addr, user, pass, Options{...})` - keep call-sites using the `Options` struct rather than positional bools, and pass the parent context so the dial honours cancellation. Set `Options.Label`. It prefixes the log lines and the errors of LIST, CAPABILITY, CREATE and APPEND. It also prefixes the EXAMINE, STATUS and FETCH errors of the sync scan and copy, and the error of a reconnect give-up. So a LIST or a CAPABILITY that fails inside `New` names the side. The errors of the dial, the greeting, LOGIN and the `show` statistics have no label.
- Dependencies are vendored under `vendor/`. The module requires Go 1.27.3 or newer; an older host toolchain is refused unless `GOTOOLCHAIN` is left on `auto` to fetch one.
- Progress output uses one `progress.Writer` for each phase. `Writer.Stop` waits for the last render and erases nothing. So the last line of each tracker stays on screen. Each log that a caller queues before the last tracker ends also stays. Print a line to `out` before `Start` or after `Stop`. While a writer runs, use its `Log`, because the next redraw can erase a line that appears in `out`.
- A tracker that gets its final text or mark after its last `Increment` sets `AutoStopDisabled`. Without it, the last `Increment` ends the tracker, and a later text or mark does not show. A tracker that the code finishes before `AppendTracker` sets `DeferStart`, because `AppendTracker` restarts a tracker and clears its marks.
- `progress.NewWriter` sets an `Indeterminate` bar indicator on every `Writer`, also on a quiet one. go-pretty calls the indicator for a tracker with a total of 0 or a value above its total. A nil indicator panics the render goroutine.
  - The indicator keeps state between renders, so each `Writer` builds its own. A determinate bar does not change.
- `show --quiet` writes no colour codes. go-pretty keeps one colour flag for the whole process, and the environment sets it. So `newShowColors(quiet)` gives plain colours for the table title and the totals in quiet mode, and the flag does not change. `show` sets `AutoStopDisabled` on its two trackers and calls `Stop`, so their last lines stay above the tables. Each tracker ends with the done mark or the error mark, also when an account fails, so no row stays in progress.

## Multi-agent workflow

This repo ships a `.claude/` directory with five role-specific subagents and a shared rule set. They exist to keep non-trivial changes architecturally honest:

- **architect** (opus) - gatekeeper. Validates the brief, may reject, orchestrates the rest.
- **developer** (sonnet) - implements within a precise brief; writes WHY-comments and unit tests.
- **tester** (sonnet) - runs the suite, audits coverage and invariants, proposes deliberate exclusions.
- **security** (opus) - CVE + attack-surface audit; reasons about combined risk.
- **tech-writer** (sonnet) - comment hygiene + CLAUDE.md / README.md / `config.example.*` sync.

Entry points:

- `/architect <task>` - hand the task to the architect (the architect decides whether to proceed).
- `/workflow <task>` - full architect-led delegation chain (preferred for non-trivial changes).
- `/dev`, `/qa`, `/sec`, `/docs` - invoke a single agent directly (use when you know which step you need).
- `/deps`, `/lint`, `/test`, `/check`, `/fix`, `/build`, `/build-linux`, `/oci`, `/run` - thin wrappers over the `Justfile` targets.

The rules each agent reads live in `.claude/rules/`:

- [workflow.md](.claude/rules/workflow.md) - delegation protocol (architect-led + fallback).
- [architecture.md](.claude/rules/architecture.md) - layering and sync-flow invariants the architect enforces.
- [go-style.md](.claude/rules/go-style.md) - Go style, stricter than community defaults.
- [testing.md](.claude/rules/testing.md) - what is tested, what is deliberately excluded.
- [security.md](.claude/rules/security.md) - standing security checklist + combined-surface checks.
- [docs.md](.claude/rules/docs.md) - comment + doc audit rules.

Skills in `.claude/skills/` (auto-loaded by agents when relevant): `idiomatic-go-review`, `imap-sync-correctness`, `coverage-audit`, `security-audit`, `doc-sync`.
