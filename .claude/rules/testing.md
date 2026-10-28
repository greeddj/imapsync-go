# Testing rules

## What "covered" means here

- A change is covered when every behavioural branch the change introduced is exercised by a test that would fail if the branch broke.
- Coverage % is a guideline, not a gate. Coverage of error paths and invariants matters more than total %.
- `go test -cover -coverprofile=cover.out ./...` and `go tool cover -func=cover.out` are the source of truth.

## What we test

- **`internal/config`** - the configuration loader.
  These tests cover:
  - loading and validation in both extensions, the defaults (worker clamp, rate-limit nil case) and the accepted and rejected `auth` values;
  - unknown keys at every level in both formats;
  - content after the first document, and empty and null YAML documents after the first document;
  - empty files, and the load of both example files;
  - rejected `map` items (an empty or missing `src` or `dst`, and a null YAML item), where the error names the item and the key;
  - a folder name with spaces, which loads without change, and an empty or null `map`, which loads with no items;
  - a rate-limit flag that the user sets, which replaces the config value also when the flag is 0;
  - a rate-limit flag that the user does not set, which keeps the config value.
- **`internal/client`** - error classification, mailbox cache update on create (no new LIST), fetch batching boundaries and AppendMessage streaming (no ReadAll).
  Error paths and cancellation run against scripted fake servers. These tests cover:
  - a refusal of each command;
  - a context that ends while LIST, STATUS, FETCH, CREATE or APPEND waits for a reply;
  - CRAM-MD5 failure and success;
  - an unknown auth mechanism that `New` refuses before it dials;
  - a mailbox cache that keeps the names that LIST marks `\Noselect` or `\NonExistent`, in any letter case;
  - a reload of the mailbox cache or a retried LIST, which replaces the set of `\Noselect` names;
  - `CreateMailbox` for a child of a `\Noselect` parent, which sends CREATE for the child only;
  - `ListMailboxes` for a folder whose STATUS or size read fails: the folder carries the reason in `Err`, and the other folders keep their values;
  - `ListMailboxes` for a `\Noselect` name, which gets no command and no error;
  - `ListMailboxes` for a reconnect that gives up in the middle of the list;
  - `FetchMessageMap`, which marks only a server refusal of EXAMINE with `ErrSelectRefused` and does not mark a dropped session;
  - a very large message count from EXAMINE or STATUS, which must not cause a large allocation (`Test_FetchMessageMap_When_ServerReportsHugeMessageCount_Then_AllocationStaysSmall`);
  - `withCancel` after stop, and shared limiters that draw from one budget.

  The hierarchy delimiter has these tests:
  - `Test_loadMailboxCache_When_ListNamesNoDelimiter_Then_DelimiterStaysEmpty` - NIL entries leave the cache delimiter empty, and the first delimiter that an entry names sets it;
  - `Test_CreateMailbox_When_ServerIsFlat_Then_SendsOneCreateWithoutParents` and `Test_ListSubfolders_When_ServerIsFlat_Then_ReturnsNoChildren` - a flat server gets one CREATE for the whole name, and no name is a child;
  - `Test_loadMailboxCache_When_ListIsRetried_Then_DelimiterOfTheFailedAttemptIsDropped` - a retried LIST keeps no delimiter of the failed attempt.

  The INBOX letter case has these tests:
  - `Test_canonicalName`, `FuzzCanonicalName` and `Test_canonicalName_When_NameNeedsNoChange_Then_AllocatesNothing` - the fold of a leading INBOX level follows the cache delimiter; a name that needs no fold costs no allocation;
  - `Test_ResolveName` and `Test_ResolveName_When_NameNeedsNoFolding_Then_LookupsAllocateNothing` - the listed spelling, and a lookup that needs no fold costs no allocation;
  - `Test_loadMailboxCache_When_ListGivesMixedCaseInboxChild_Then_CacheKeepsListedName` and `Test_loadMailboxCache_When_ListGivesTwoSpellingsOfAnInboxChild_Then_CacheKeepsAllNames` - the cache keeps each listed spelling;
  - `Test_loadMailboxCache_When_EntryNamesAnotherDelimiter_Then_InboxLevelFollowsTheCacheDelimiter` and `Test_loadMailboxCache_When_SlashChildIsListedOnADotServer_Then_InboxLevelDoesNotFold` - the fold uses the cache delimiter only;
  - `TestAddMailboxToCache_When_TwoSpellingsShareOneCanonicalForm_Then_SmallerWinsInAnyOrder` and `Test_loadMailboxCache_When_ListIsRetried_Then_SpellingsOfTheFailedAttemptAreDropped` - the indexed spelling does not depend on the order, and a retry drops the spellings of a failed attempt;
  - `Test_MailboxExists_When_NameIsInboxInAnyCase_Then_CacheFindsIt`, `Test_MailboxExists_When_ServerIsFlat_Then_OnlyTheBareInboxNameFolds`, `Test_ListSubfolders_When_ParentIsInboxInAnyCase_Then_ReturnsInboxChildren`, `Test_IsNoSelect_When_NameDiffersOnlyInInboxCase_Then_ReportsIt` and `Test_CreateMailbox_When_NameIsInboxInAnotherCase_Then_SendsNoCreateForInbox` - each cache read and `CreateMailbox` follow the fold;
  - `Test_folderLock_When_NamesDifferOnlyInInboxCase_Then_ShareOneMutex` - two spellings of one name share one lock;
  - `Test_FetchMessageMap_When_ListGivesInboxChildInAnotherCase_Then_ExaminesTheListedName` - the EXAMINE carries the listed spelling;
  - `Test_CacheReads_When_ContextAlreadyEnded_Then_ReturnContextError` - the cache reads return the context error and send no new LIST.

  The names that are not valid modified UTF-7 have these tests:
  - `Test_New_When_ListHoldsNamesThatAreNotModifiedUTF7_Then_CacheLeavesThemOut` and `Test_loadMailboxCache_When_ListSendsInvalidNameAsLiteral_Then_LeavesItOutAndKeepsTheNextEntry` - the cache leaves out each such name, a literal too, and keeps the next entry;
  - `Test_New_When_ListIsInterleavedWithOtherUntaggedResponses_Then_CacheKeepsEveryName` - other untagged responses do not drop a name;
  - `Test_New_When_ListEntryLacksAName_Then_FailsAsBefore` and `Test_New_When_ListEntryIsMalformed_Then_FailsInsteadOfLeavingItOut` - any other parse error still fails the LIST, and the error names the label;
  - `Test_InvalidMailboxNames_When_ListNamesOneInvalidNameTwice_Then_ReportsItOnceAndReturnsACopy` - each name shows once, and the caller gets a copy;
  - `Test_loadMailboxCache_When_Reloaded_Then_InvalidNamesAreReplaced`, `Test_ListMailboxes_When_ListHoldsInvalidName_Then_ResultLeavesItOutAndRecordsIt` and `Test_ListMailboxes_When_ListChangesAfterConnect_Then_InvalidNamesFollowTheLatestList` - the latest LIST sets the left-out names, for the cache and for `ListMailboxes`;
  - `Test_loadMailboxCache_When_ListIsRetried_Then_NamesAndInvalidNamesOfTheFailedAttemptAreDropped`, `Test_ListMailboxes_When_ListIsRetried_Then_ResultAndInvalidNamesOfTheFailedAttemptAreDropped` and `Test_listAll_When_CommandWriteFails_Then_ReturnsWhileCallbackStillRuns` - a failed attempt leaves no state, also when go-imap calls the callback after `Execute` returns;
  - `Test_ListMailboxes_When_SessionEndedByBye_Then_ReconnectsAndLists` - after a BYE, `ListMailboxes` reconnects and lists.

  The error class has these tests:
  - `TestClassifyError` and `TestClassifyError_When_WrapperTextHoldsAPhrase_Then_LeafDecides` - the text of a wrapper (a folder name or a label) never changes the class, and a leaf reply keeps its class under each wrapper;
  - `TestCanRevive` - only a transient or throttled give-up can revive;
  - `TestIsAlreadyExistsErr` - the CREATE refusal texts, with the Gmail text `Duplicate folder name`;
  - `Test_FetchMessageMap_When_ConnectionDropsAndNameHoldsThrottlePhrase_Then_ReconnectsAndRetries` and `Test_StreamMessagesByUIDs_When_ConnectionDropsAndNameHoldsThrottlePhrase_Then_ReconnectsAndStreams` - a folder name that holds a throttle phrase does not stop the reconnect;
  - `Test_FetchMessageMap_When_RefusalEchoesTransientPhraseOfFolderName_Then_ReconnectsOnce` - a refusal whose text holds a transient phrase because it echoes the folder name costs one reconnect, and then `FetchMessageMap` returns the refusal;
  - `Test_CreateMailbox_When_RetriedCreateAnswersDuplicateFolderName_Then_ReportsExisting` - a CREATE that the server applied before it dropped the connection does not fail when the retry gets the Gmail reply.

  The reconnect attempts have these tests:
  - `Test_safeCall_When_ReconnectLoginFailsTemporarily_Then_RetriesAndRecovers` - a temporary authentication failure gets the backoff, and the next attempt restores the session;
  - `Test_reconnect_When_LoginAlwaysFailsTemporarily_Then_StopsAfterMaxAttempts` - a temporary failure uses all attempts and never waits for the throttle cool-down;
  - `Test_reconnect_When_LoginFailsWithBadCredentials_Then_GivesUpAfterOneAttempt` - a refusal of the credentials ends `reconnect` at the first attempt;
  - `Test_reconnect_When_EveryAttemptFails_Then_NoWaitFollowsTheLastAttempt` - `reconnect` waits only between two attempts;
  - `Test_reconnect_When_LastAttemptFailsPermanently_Then_ReturnsTheRefusal` and `Test_reconnect_When_LastAttemptSucceeds_Then_Reconnects` - the last attempt still runs, and its result is the result of `reconnect`.

  The give-up and revive gate has these tests:
  - `Test_safeCall_When_WithinReviveInterval_Then_FailsFastWithoutDial` - inside `reviveInterval`, a call returns the stored give-up with no dial, login attempt, wait or log line;
  - `Test_safeCall_When_ReviveIntervalPassed_Then_OneLoginRestoresTheSession` - after `reviveInterval`, one login attempt restores the session and the full reconnect budget;
  - `Test_safeCall_When_ReviveProbeFails_Then_OneAttemptAndCooldownRestarts` - a failed attempt is one login with no backoff, and `reviveInterval` starts again;
  - `Test_safeCall_When_ReconnectWasRefusedPermanently_Then_LaterCallsSendNoLogin` - a permanent give-up is final, also when `reviveDur` is zero;
  - `Test_safeCall_When_ReconnectGaveUpOnUnknownRefusal_Then_LaterCallsSendNoLogin` - an unknown-class give-up is final;
  - `Test_safeCall_When_CancelledDuringReviveProbe_Then_ReturnsCanceledAndKeepsGiveUp` - a cancelled login attempt returns `context.Canceled` and keeps the earlier give-up;
  - `Test_safeCall_When_EarlierReconnectGaveUp_Then_NextCallWaitsAndReconnects` - after a give-up that can revive, the next call waits until the reconnect interval ends, logs in once and runs `fn` once;
  - `Test_safeCall_When_ReviveProbeIsRefusedPermanently_Then_NoFurtherLogin` - a permanent refusal of the login attempt is final;
  - `Test_safeCall_When_RevivedSessionFailsTransiently_Then_DoesNotReconnectAgain` - a call that reconnected first runs `fn` one time, and the transient error goes to the caller;
  - `Test_safeCall_When_CancelledWithoutSession_Then_ReturnsCanceledWithoutDial`, `Test_reconnect_When_CancelledWhileWaitingForLock_Then_ReturnsCanceledNotTheGiveUp` and `Test_safeCall_When_CancelledDuringWaitBeforeReconnect_Then_ReturnsCanceledWithoutLogin` - a cancel comes before the stored give-up, ends the wait, and no LOGIN follows;
  - `Test_safeCall_When_CancelledDuringReconnectWait_Then_ReturnsCanceledWithoutAnotherLogin` - a cancel ends the wait between two reconnect attempts, no LOGIN follows, `fn` does not run again and `c.mu` is free;
  - `Test_reconnect_When_ClientIsCancelled_Then_ReturnsCanceledWithoutDial` - a cancelled Client neither dials nor logs in, and `c.mu` is free;
  - `Test_safeCall_When_CancelledRightAfterReconnect_Then_ReturnsCanceledWithoutRunningFn` and `Test_AppendMessage_When_CancelledRightAfterReconnect_Then_SendsNoAppend` - a cancel that comes right after a reconnect returns `context.Canceled`, `safeCall` does not run `fn` again, and `AppendMessage` sends no APPEND;
  - `Test_safeCall_When_SessionMissingAfterReconnect_Then_ReturnsErrorWithoutRunningFn` - when a reconnect leaves no session, `safeCall` returns an error and does not run `fn` again;
  - `Test_Client_When_EarlierReconnectGaveUp_Then_OperationsReconnect` - each public operation that uses `safeCall` logs in again on a Client that has no session;
  - the "account lock" and "account lockout" cases of `Test_safeCall_When_ServerStillRefusesAfterGiveUp_Then_EachCallSpendsOneAttempt` - after an account-lock reply, the Client sends no further login;
  - `Test_AppendMessage_When_RepairGaveUpTransiently_Then_NextAppendProbesAfterCooldown` - `AppendMessage` goes through the same gate.

  The `selectIfNeeded` check has these tests:
  - `Test_selectIfNeeded_shortCircuitsOnNetwork` - a second call for the selected folder sends no EXAMINE;
  - `Test_selectIfNeeded_When_FolderDiffers_Then_SelectsAgain` - a different folder gets a new EXAMINE;
  - `Test_selectIfNeeded_When_SessionEndedByBye_Then_DoesNotShortCircuit` - after a BYE, the call does not use the old selection;
  - `Test_StreamMessagesByUIDs_When_ExamineOfAnotherFolderFailed_Then_SelectsAgain` - after a failed EXAMINE of another folder, the earlier folder gets a new EXAMINE;
  - `Test_selectIfNeeded_afterReconnect_reSelects` - the session after a reconnect gets a new EXAMINE;
  - `Test_safeCall_When_ByeLandsBetweenSelectAndFetch_Then_ReconnectsAndRetries` - a BYE between the EXAMINE and the FETCH gets a reconnect, and the retry selects again;
  - `Test_StreamMessagesByUIDs_When_ServerSendsByeBetweenBatches_Then_ReconnectsAndDeliversTheRest` - a BYE after the first batch gets a reconnect, the next batch selects the folder on the new session, and the stream does not fetch a delivered UID again;
  - `Test_StreamMessagesByUIDs_When_StreamSpansBatches_Then_SelectsFolderOnce` - all batches of one stream share one EXAMINE, also for a non-ASCII name that go-imap sends in modified UTF-7;
  - `Test_StreamMessagesByUIDs_When_SessionEndedAfterEarlierSelect_Then_ReselectsOnANewSession` - the new session does not keep a selection made before a BYE: the stream logs in again, selects again and delivers each UID once.

  The APPEND retry has these tests:
  - `Test_AppendMessage_When_SessionEndedWhileIdle_Then_ReconnectsAndAppends` - a session that ended with a BYE or a closed socket gets a reconnect before the APPEND;
  - `Test_AppendMessage_When_FailsBeforeBodyIsRead_Then_RetriesOnce` - a transient failure before go-imap reads the body gets one reconnect and one APPEND, and the first connection gets no APPEND;
  - `Test_AppendMessage_When_BodyWasSent_Then_RepairsWithoutRetry` - after go-imap read the body (an empty body too), the call repairs the session and sends no second APPEND;
  - `Test_AppendMessage_When_ServerStopsReadingLiteral_Then_WatchdogTripsAndRepairs` - a failure in the middle of the literal gets a repair and no second APPEND;
  - `Test_AppendMessage_When_RevivedSessionFailsBeforeBodyIsRead_Then_DoesNotReconnectAgain`, `Test_AppendMessage_When_RevivedSessionFailsTransiently_Then_DoesNotReconnectAgain` and `Test_AppendMessage_When_RetryGoesSilent_Then_WatchdogTripsWithoutAnotherReconnect` - one call reconnects a maximum of one time;
  - `Test_AppendMessage_When_RetryReconnectIsRefused_Then_ReturnsReconnectError` - a refused login for the retry returns the reconnect error, and the next call returns the same refusal with no new login;
  - `Test_AppendMessage_When_ReconnectBeforeAppendIsRefused_Then_ReturnsReconnectError` - when the server ended the session while idle and refuses each login attempt, `AppendMessage` returns the reconnect error and sends no APPEND;
  - `Test_AppendMessage_When_ContextEndsDuringRetry_Then_ReturnsContextError` - a context that ends while the retry waits returns its error, with no further reconnect;
  - `Test_AppendMessage_When_ContextEndsDuringReconnectBeforeAppend_Then_ReturnsContextError` and `Test_AppendMessage_When_ContextEndsDuringRetryReconnect_Then_ReturnsContextError` - a context that ends while a login waits returns the context error, not the reconnect error, and no APPEND reaches a session;
  - `Test_AppendMessage_When_RetriedAfterEarlyFailure_Then_WireMatchesADirectAppend` - the retry sends the same flags, date and bytes as an APPEND that needed no retry, and exactly one complete literal reaches the server;
  - `Test_readTracker` - `readTracker` marks a body as read only through `Read`, and it does not offer `WriteTo`, so `io.Copy` cannot move the body without a `Read`;
  - `Test_AppendMessage_When_RepairWasRefusedPermanently_Then_NextAppendSendsNoLogin` and `Test_AppendMessage_When_ReconnectWasRefusedPermanently_Then_LaterAppendsSendNoLogin` - after a permanent refusal, `AppendMessage` sends no LOGIN and no APPEND.
- **`internal/ratelimit`** - token starvation, nil-limiter pass-through, read+write directions independent.
- **`internal/utils`** - `AskConfirm` and its helpers. The tests cover the `-y/--confirm` flow. `AskConfirm` reads each answer from one shared buffered reader.
- **`internal/app`** - sync plan diff (Message-Id intersection), subfolder expansion, delimiter reconciliation, worker pool dispatch.
  The `runSync` and `runShow` tests run end to end against a fake IMAP server that holds mailboxes in memory. These tests cover:
  - the plan-confirm-execute order, and the rule that `--quiet` needs `--confirm`;
  - an idempotent second run, and the delimiter rewrite on both sides;
  - nested folder creation, and one rate-limit budget per direction that all workers share;
  - a read-only source (no write commands, no `\Seen` change);
  - a cancel at each boundary between sync phases;
  - the refusal paths (login, LIST, EXAMINE, CREATE, APPEND);
  - a source that drops the connection on a folder with a throttle phrase in its name, where the folder is still copied (`TestRunSync_When_SourceDropsOnFolderNamedLikeThrottle_Then_FolderIsStillCopied`);
  - a CREATE that gets the Gmail reply `Duplicate folder name`, where the sync completes (`TestRunSync_When_CreateAnswersDuplicateFolderName_Then_SyncCompletes`);
  - a destination that refuses LOGIN in the middle of a run, where the worker sends one refused LOGIN (`TestRunSync_When_DestinationRefusesLoginMidRun_Then_WorkerSendsOneRefusedLogin`);
  - a source folder that fails its scan: a refused EXAMINE, a mapping with a typo, a missing folder and a planning client that gives up;
  - a source scan failure, where the other folders still copy, the failure list shows also with `--quiet`, and the run returns `ErrSilentExit`;
  - a run with failed folders and nothing to copy, where the sync asks no question and ends non-zero;
  - a `\Noselect` source folder, where only the subfolders copy;
  - a `\Noselect` destination, where a source message with a Message-Id gives a folder failure and an empty source folder gives none;
  - a mirror run where both servers list the Gmail container `[Gmail]`;
  - a readable source folder that LIST does not show, where the folder and its subfolders copy;
  - an unlisted parent that refuses to open, where the plan skips it, prints one line and keeps the exit code (`isHierarchyLevel`);
  - an unlisted INBOX with subfolders that refuses to open, which stays a failure;
  - an unlisted parent whose session drops, which stays a failure;
  - a destination that CREATE reports as existing but LIST did not show, where the run copies only the missing messages and a second run sends no APPEND, opens no worker connection and sends one LIST (`TestRunSync_When_CreateReportsExistingFolderWithMessages_Then_CopiesOnlyTheDifference`);
  - the same check for an empty existing destination, which sends no source command (`TestRunSync_When_ExistingDestinationIsEmpty_Then_SourceIsNotScannedAgain`), and for a folder that CREATE made, which gets no scan (`TestRunSync_When_CreateSucceeds_Then_NewFolderIsNotScanned`);
  - a destination LIST that has names that are not valid modified UTF-7, where a folder that CREATE made is also checked. The check copies only the missing messages when the folder holds some (`TestRunSync_When_DestinationListsInvalidNameAndCreateAnswersOKForFolderWithMessages_Then_CopiesOnlyTheDifference`). The check reads only the destination when the folder is empty (`TestRunSync_When_DestinationListsInvalidNameAndCreateMakesFolder_Then_RecheckReadsOnlyTheDestination`);
  - a destination folder that LIST does not show, where the run copies only the missing messages and checks only that folder (`TestRunSync_When_DestinationInboxIsMissingFromList_Then_OnlyTheDifferenceIsCopied` and `TestRunSync_When_OnlyOneDestinationIsMissingFromList_Then_OnlyThatFolderIsChecked`);
  - the same check with `--quiet`, where the run copies the missing messages and prints no "already exists and holds" line (`TestRunSync_When_ExistingDestinationHoldsMessagesInQuietMode_Then_PrintsNoInfoLine`);
  - an existing destination that cannot be scanned, where the folder failure shows also with `--quiet`, the other plans still copy, and the run opens no worker connection when no plan is left (`TestRunSync_When_ExistingDestinationCannotBeScanned_Then_ReportsFolderFailure`);
  - a source that cannot be scanned again during the check, which gives a folder failure and no APPEND (`TestRunSync_When_SourceRescanFails_Then_ReportsFolderFailure`);
  - a context that ends during the check, where `runSync` returns `context.Canceled`, sends no APPEND and prints no failure list (`TestRunSync_When_ContextEndsDuringDestinationRecheck_Then_ReturnsCanceledWithoutAppend`);
  - a context that has ended before `recheckExistingDestinations` starts, where a plan outside the set passes and the first plan in the set sends no command, and the call returns `context.Canceled` with no kept plan and no failure (`Test_recheckExistingDestinations_When_ContextAlreadyEnded_Then_SendsNoCommand`);
  - a source that changes between the plan and the check (`TestRunSync_When_SourceChangesAfterTheCreateReply_Then_OnlyPlannedMessagesAreCopied`): new mail is not copied, and a planned message that leaves the source is not copied. For new mail, the run prints the "holds 2 of the 3" line and no "no longer holds" line. For a message that leaves the source, it prints the "holds 2 of the 3" line and the "no longer holds 1 of the 3" line, and never "holds 3 of the 3";
  - `Test_recheckExistingDestinations_When_PlannedMessagesAreDropped_Then_EachCauseHasItsOwnLine` - the exact output and the kept plans for these cases: the destination holds some messages, the source lost one, both causes, nothing left, both causes with `--quiet` (no output), nothing dropped (no output), and folder names with control characters (escaped output and exact plan names);
  - `Test_rediffPlan` - the new diff never adds a UID, drops the UIDs that the destination holds or the source lost, scales the size and leaves the input slice unchanged. For each case, `held` and `gone` have the expected values, and `held`, `gone` and the kept UIDs add up to the planned UIDs;
  - a context that ends before either scan of `buildSyncPlan` starts, where `buildSyncPlan` returns `context.Canceled` (`Test_buildSyncPlan_When_ContextEndsAfterEntryCheck_Then_NeitherScanStarts`);
  - a context that ends before the hierarchy check of a refused source folder, where `buildSyncPlan` returns `context.Canceled` (`Test_buildSyncPlan_When_ContextEndsBeforeHierarchyCheck_Then_ReturnsContextError`);
  - `runShow` with unreadable folders (marked rows, the reason list and a non-zero exit) and with a `\Noselect` container (a `container` row and exit code 0);
  - the `ActionSync` and `ActionShow` adapters, which run through a urfave/cli root (the TLS-only dial and the config load errors);
  - a config with an unknown key, an unsupported `auth` value or a `map` item with no folder name, which both adapters refuse before they connect.

  The delimiter fix has these tests:
  - `Test_delimiterRewrite` and `Test_delimiterRewrite_When_TypedNameAndItsRewriteAreBothListed_Then_KeepsTheTypedName` - a rewrite is offered only for an unlisted name with a listed result; a listed name never changes;
  - `Test_delimiterRewrite_When_ContextEnded_Then_ReturnsContextError`, `Test_delimiterRewrite_When_ContextEndsBetweenLookups_Then_ReturnsContextError` and `Test_collectDelimiterFixes_When_ContextEndsDuringLookup_Then_ReturnsContextError` - a context that ends returns its error;
  - `TestRunSync_When_TypedNameIsListed_Then_NoDelimiterPrompt` - a listed name gives no question;
  - `TestRunSync_When_TypedNameAndItsRewriteAreMissing_Then_NameIsKeptWithoutPrompt` - an unlisted name with no listed rewrite stays as typed, on the source side and on the destination side, with no question;
  - `TestRunSync_When_TypedNameAndItsRewriteAreBothListed_Then_OnlyTheTypedFolderIsCopied` - the typed folder is copied and its rewrite is not;
  - `TestRunSync_When_DestinationListsTheRewrittenPath_Then_PathIsRewrittenAfterConfirmation` - the rewrite happens after the answer, or with `-y`;
  - `TestRunSync_When_DelimiterIsRewritten_Then_ConfigMapKeepsTheTypedNames` - the config `map` of the caller does not change;
  - `TestRunSync_When_MirroredNameHoldsDelimiterText_Then_FolderIsCopiedUnchanged` - a mirror run asks no question and keeps a name that holds delimiter text;
  - `TestRunSync_When_TypedSourceNamesInboxChildInAnotherCaseWithWrongDelimiter_Then_RewriteTargetsTheListedFolder` - the rewrite finds a listed INBOX child in another letter case.

  The name conversion and the rename notice have these tests:
  - `Test_convertFolderPath`, `Test_convertFolderPath_When_AnyDelimiterPair_Then_LevelsMatchTheSource` and `FuzzConvertFolderPath` - the rules of `convertFolderPath`, and the levels of the result for any pair of delimiters;
  - `Test_expandMappingsWithSubfolders_When_SegmentHoldsDestinationDelimiter_Then_ItBecomesUnderscore` and `Test_expandMappingsWithSubfolders_When_RootIsRenamed_Then_SubfoldersAreRenamed` - the rename of a level, and a root that passes the rename to its subfolders;
  - `Test_expandMappingsWithSubfolders_When_ListOrderVaries_Then_SubfoldersFollowByteOrder` - the order of the subfolders does not depend on the LIST order;
  - `Test_expandMappingsWithSubfolders_When_MirrorRootIsExpanded_Then_EachSubfolderAgreesWithMirrorMappings` - an expanded subfolder and its mirror mapping give the same destination;
  - `Test_expandMappingsWithSubfolders_When_ListedChildSpellsInboxInAnotherCase_Then_RelativePathIsCutByLength` - the relative path of an INBOX child in another letter case;
  - `TestRunSync_When_SourceSegmentHoldsDestinationDelimiter_Then_FolderIsRenamedNotNested`, `TestRunSync_When_MirrorCrossesDelimiters_Then_DestinationUsesItsDelimiterWithoutPrompt` and `TestRunSync_When_TypedDestinationHoldsDestinationDelimiter_Then_NameIsNotRenamed` - the end-to-end results, and a typed destination that stays as typed;
  - `TestRunSync_When_FolderIsRenamed_Then_WarningShowsWithoutVerboseAndBeforePrompt` - the rename notice shows before the prompt, also without `--verbose` and with `--quiet`;
  - `TestRunSync_When_RenamedNameCarriesControlCharacters_Then_WarningShowsEscapesAndServerGetsExactName` - the notice escapes the names, and the servers get the exact names.

  The clash of destination names has these tests:
  - `Test_splitDestinationClashes` - a group with a renamed mapping fails every member, and a group with no renamed mapping stays;
  - `TestRunSync_When_RenameMakesTwoSourcesShareADestination_Then_BothFailAndNothingMerges` - both folders fail, the run opens neither folder on the source, and it creates no destination folder for them;
  - `TestRunSync_When_RenameClashAppearsAfterAnEarlierRun_Then_ExistingFolderGetsNoForeignMail` - a folder that an earlier run filled gets no mail from a renamed source;
  - `TestRunSync_When_RenameClashNamesCarryControlCharacters_Then_FailureLinesShowEscapesAndNothingIsSent` - the failure lines escape the names.

  The INBOX letter case in `sync` has these tests:
  - `Test_resolveMappings` - each typed name gets the listed spelling;
  - `TestRunSync_When_DestinationNamesInboxInAnotherCase_Then_NothingIsCopiedTwice` and `TestRunSync_When_SourceNamesInboxInAnotherCase_Then_SubfoldersAreCopied` - a typed `inbox` selects the INBOX and its subfolders;
  - `TestRunSync_When_SourceListsInboxChildInMixedCase_Then_ChildIsCopiedOnce` and `TestRunSync_When_SourceListsTwoSpellingsOfAnInboxChild_Then_BothAreScannedAndCopied` - each listed spelling is one folder;
  - `TestRunSync_When_SourceListsInboxChildInAnotherCaseAndMatchesItExactly_Then_WireKeepsTheListedName`, `TestRunSync_When_DestinationListsInboxChildInAnotherCase_Then_ItIsUsedUnderItsListedName`, `TestRunSync_When_DestinationListsInboxChildWithNilDelimiter_Then_ItIsUsedUnderItsListedName` and `TestRunSync_When_SourceListsInboxChildWithNilDelimiter_Then_TypedNameScansTheListedFolder` - the commands carry the listed spelling;
  - `TestRunSync_When_TwoSourcesDifferOnlyInInboxCase_Then_TheyAreOneSource` - two typed spellings of one source are one source;
  - `TestRunSync_When_DestinationNameDiffersInCaseFromAnotherFolder_Then_ItStaysASeparateFolder` - a name that is not INBOX keeps its letter case.

  The flat servers have these tests:
  - `TestRunSync_When_BothServersAreFlat_Then_DottedFolderIsCopiedUnchanged` and `TestRunSync_When_BothServersAreFlat_Then_TypedNamesWithDelimiterTextAreCopiedUnchanged` - a name with delimiter text stays;
  - `TestRunSync_When_DestinationIsFlat_Then_CreatesNoParentFolder` and `TestRunSync_When_DestinationListsNothing_Then_NestedTypedNameIsCreatedWithoutParent` - a flat destination, or one that lists nothing, gets no parent folder;
  - `TestRunSync_When_SourceIsFlat_Then_MappingGetsNoSubfolders` and `TestRunSync_When_FlatSourceHoldsNameWithSlash_Then_UnlistedParentIsAFailureNotASkip` - a flat source has no subfolders, so an unlisted name that refuses to open is a folder failure, not a skip;
  - `TestRunSync_When_DestinationIsFlat_Then_SubfoldersKeepTheirSourceNames` - a subfolder keeps the delimiter of the source.

  The names that are not valid modified UTF-7 have these tests:
  - `TestRunSync_When_UnmappedFolderNameIsNotModifiedUTF7_Then_RestSyncsAndWarns` and `TestRunSync_When_MirrorSourceListsInvalidName_Then_OtherFoldersCopyAndWarningShowsWithQuiet` - the run copies the other folders and prints the warning, also with `--quiet`;
  - `TestRunSync_When_DestinationListsInvalidName_Then_SyncRunsAndWarnsWithDstLabel` and `TestRunSync_When_BothSidesListInvalidNames_Then_SourceWarningsComeFirstInNameOrder` - the label and the order of the warnings;
  - `TestRunSync_When_MappingNamesInvalidFolder_Then_FolderFailsAndRestCopies` - a mapping that names such a folder gives a folder failure;
  - `TestRunSync_When_MirrorListsNamesThatAreNotModifiedUTF7_Then_NoCommandCarriesTheirBytes` - no command carries the raw bytes of a left-out name;
  - `TestRunSync_When_ListHoldsNameWithLatin1Byte_Then_RunSucceedsAndShowsTheEscapedName` - a name with an invalid UTF-8 byte shows as escape text;
  - `TestRunShow_When_FolderNameIsNotModifiedUTF7_Then_TableLeavesItOutAndWarns` and `TestRunShow_When_FolderNameHoldsLatin1Byte_Then_TableLeavesItOutAndWarnsWithTheEscapedName` - `show` leaves the folder out of the table and sends no STATUS for it; it prints the warning after the tables, with and without `--quiet`.

  The label of an error has these tests:
  - `TestRunSync_When_SourceListFailsAtConnect_Then_ErrorNamesTheLabel`, `TestRunSync_When_DestinationListFailsAtConnect_Then_ErrorNamesTheLabel` and `Test_newSyncWorkerPool_When_WorkerListFails_Then_ErrorNamesTheWorkerLabel` - a LIST that fails inside `client.New` names the side or the worker.
- **`internal/progress`** - interface compliance against the contract `internal/client` expects.
- **`internal/termsafe`** - `Escape` and `Writer` have 100% coverage. The tests are table tests, a fuzz target, `Writer` tests and a test that `Escape` does not allocate memory when nothing needs escaping.
- **Terminal output in `internal/client` and `internal/app`** - end-to-end tests with fake servers use a folder name, a reply and a Message-Id that contain control and bidi characters. They check the escaped output, and they check that the names on the wire stay byte-exact. These tests include the list of folders that cannot be synced, the skip line, and the list of folders that `show` cannot read. They also include the left-out lines, the rename notice and the "already exists and holds" lines (`TestRunSync_When_ExistingDestinationNamesCarryControlCharacters_Then_InfoLineShowsEscapesAndServersGetExactNames`).
- **`cmd/imapsync-go`** - `TestReportError` covers the exit code and the escaped `Error:` line. `TestFetchTag` covers `helpers.fetchTag` against an `httptest` server.

## What we deliberately don't test

If a behaviour is hard to test, test it at integration level. If you cannot do that, record it in this file as a deliberate exclusion and give the reason. These are examples of acceptable exclusions:

- The TLS handshake itself (delegated to stdlib).
- `goreleaser` output (delegated to goreleaser).
- The `urfave/cli/v3` flag parsing wiring in `cmd/imapsync-go/main.go` (declarative; failure mode is a CLI parse error from the library).

### Deliberate exclusions in `internal/client`

- **`client.New`** - the `Test_New_*` tests cover CRAM-MD5, dial failure, LIST refusal, cancel during LOGIN and shared limiters. Only `internal/app` tests cover two branches: the `tls.Dialer` path and the default dial timeout. The `tls.Dialer` test uses a plaintext server, so a handshake error is expected. The standard library handles a successful TLS handshake, so no test covers it.
- **`Client.SetProgressTracker`** - symmetric with `SetProgressWriter`; both are nil-and-non-nil atomic.Pointer stores already exercised by `SetProgressWriter` tests.
- **`realSleepCtx`** - production sleep implementation. Tests substitute the package-level `sleepCtx` var; the real implementation is stdlib `time.NewTimer` and its correctness is delegated to stdlib.
- **Post-success `ctx.Err()` re-checks** - each check runs after a call succeeds. `withCancel` terminates the connection asynchronously, so the outcome at that boundary is a race. The checks are in these places:
  - `StreamMessagesByUIDs`, at the top of the second and later batches;
  - `CreateMailbox`, after the parent walk;
  - `createParentFolders`, at the top of the loop;
  - `ListMailboxes`, after the LIST;
  - `getFolderSize`, after success.
- **`greet` tie between the context and the timer** - the context and `dialTimeout` can end at the same moment. Both events close the connection, so they race. Both resulting errors describe the failure correctly. No deterministic test can reach the tie. Two tests cover the separate causes:
  - `Test_New_When_ServerNeverGreets_Then_ContextCancelReturns` covers the context;
  - `Test_New_When_ServerNeverGreets_Then_FailsAfterDialTimeout` covers the timer.
- **`greet` failure paths that route the logger of a dropped client** - `greet` calls `routeErrorLog` also when it drops the client, so the reader of that client does not write to stderr. `imapclient.New` creates its logger on `os.Stderr`, and the dropped client is not reachable from a test. A check needs a capture of `os.Stderr` for the whole process. Two tests cover the routing on the success path:
  - `Test_New_ErrorLogFollowsVerbose` covers the logger output of a connected client;
  - `Test_routeErrorLog` covers the escaped lines and the quiet discard.
- **`idleConn` ordering choices** - each of these four orderings closes a window of a few instructions against a concurrent timer callback:
  - `Read` and `Write` stamp the last activity before they count themselves busy;
  - `begin` stamps before it counts the span;
  - `fire` uses `CompareAndSwap` to move the state from open to tripped;
  - `connectAndLogin` stores `c.idle` before `c.c`.

  A deterministic test needs a production hook between two atomic operations. The tests cover the behaviour on both sides of each ordering.
- **`AppendMessage` with a server that is silent between the `{N}` line and the `+` continuation** - go-imap blocks the caller on `<-w.continues`, and a closed socket does not release it. A test of this window would hang. The cause is the same as in the Known limitation in `CLAUDE.md`. Two nearby paths have tests:
  - `Test_AppendMessage_When_ServerNeverAnswersAfterLiteral_Then_WatchdogTripsAndRepairs` covers a server that is silent after the literal;
  - `Test_AppendMessage_When_ServerStopsReadingLiteral_Then_WatchdogTripsAndRepairs` covers a server that stops reading the literal.

### Deliberate exclusions in `internal/app`

- **`runFolderSync` post-append `ctx.Err()` check** - The check stops the loop when an APPEND finishes after the context is canceled. `withCancel` registers a `context.AfterFunc` that calls `Cancel()`, and `Cancel()` terminates the connection asynchronously. An APPEND that finishes at the same time can return nil, not an error. This is a race window, and the test has no deterministic hook to reach it. The `(synced, errors)` result is the same with or without the check. The check saves at most one extra iteration.
- **`app.traceTracker`** - a diagnostic that writes to stderr. It is active only when `IMAPSYNC_TRACE` is set.
- **`ActionSync` verbose `Fetching config...` line** - `ActionSync` writes it to `os.Stdout` before the config loads. It is the only output that does not go through the injected writer.
- **Boundary `ctx.Err()` re-checks** - each check reads the context of the caller between two calls that give no observable event. A test can reach such a check only with a context that ends on a given call of `Err`. That test depends on the number of `Err` calls in `internal/client` and in the code before the check, so a refactor that keeps the behaviour breaks it. `TestRunSync_When_CanceledAtPhaseBoundary_Then_NothingMoreIsSent` and `TestRunSync_When_CanceledDuringAppend_Then_NoFurtherPlanIsStarted` test the end-to-end property. The re-checks are in these places:
  - `runSync`, after the connect errgroup;
  - `runSync`, at the top of the dispatch loop;
  - `expandMappingsWithSubfolders`, in the per-mapping loop, after `getSubfolders` and in the per-subfolder loop;
  - `isHierarchyLevel`, the error return after `getSubfolders`.
- **`buildSyncPlan`, the `MailboxExists` error in the destination goroutine** - `MailboxExists` reads the mailbox cache, so it fails only when the context ends. The `gCtx.Err()` check at the top of the loop runs first. `gCtx` comes from `errgroup.WithContext`, so a test cannot replace its `Err`. So the error needs a cancel in the short time between the two calls. A deterministic test needs a production hook there. `Test_buildSyncPlan_When_ContextEndsAfterEntryCheck_Then_NeitherScanStarts` covers the check at the top of the loop.

### Deliberate exclusions in `cmd/imapsync-go`

- **`main` and `run` in `cmd/imapsync-go/main.go`** - the signal-to-context wiring and `os.Exit` need a subprocess and real signals. `TestReportError` covers the exit-code mapping through `reportError`. The tests in `cmd/imapsync-go/commands` cover the command tree. One test uses the real `sync` flags, with their environment variables, to check that a rate-limit value of 0 replaces the config value. A test sends SIGQUIT to its own process to cover `installQuitDump`, with an injected exit function.
- **`helpers.latestTag`** - the function makes a live call to the GitHub API. The tests skip it through `IMAPSYNC_OFFLINE` or `CI`. `TestFetchTag` covers `fetchTag`, which sends the request, against an `httptest` server. The other tests cover only the offline return and the `Version` format.

If the tester finds an untested branch that does not match an existing exclusion, the choice is:

1. Add a test (preferred).
2. If the branch is genuinely untestable in isolation (e.g. network races), propose an integration test or add it to the exclusions list with reasoning.

## Test commands

- `just test` - full suite.
- `go test ./internal/client -run TestFunc` - single test.
- `go test -race ./...` - race detector. Run before declaring done on anything that touches goroutines.
- `go test -cover ./internal/<pkg>` - coverage for one package.
- `go test -coverprofile=cover.out ./... && go tool cover -func=cover.out | sort -k3 -n` - coverage report sorted by % (find the holes fast).

## When a test fails

- Do not "fix" by adjusting the assertion to match observed behaviour. Find the regression.
- If the failure is in a test the developer just wrote and the new code is correct, the test is wrong - fix the test, explain in the developer→architect report.
- If the failure is in a pre-existing test, stop. Report to architect. Do not modify pre-existing tests without explicit instruction.

## Race detector

`just run` runs `go run -race`. If you introduce concurrency, run `go test -race ./...` locally and report the result.
