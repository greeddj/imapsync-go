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
  - two `map` items with one `src` and different `dst` values, which `config.New` rejects with `ErrMapSrcConflict`. The error names both items, also when other items or an equal item come between them (`TestNew_When_MapRepeatsSourceWithAnotherDestination_Then_ReturnsErrorNamingBothEntries`);
  - two equal `map` items, two items with one `dst` and two `src` values that differ only in letter case, which all load (`TestNew_When_MapItemsRepeatOrShareADestination_Then_Loads`);
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
  - `FetchMessageMap` for a response with a Message-Id and no UID or UID 0, which fails the scan with no reconnect (`Test_FetchMessageMap_When_ResponseHasMessageIdButNoUID_Then_ScanFails`);
  - `FetchMessageCopies` for the same responses, which keeps each one as a copy with UID 0. Two such responses with one Message-Id count as two copies (`Test_FetchMessageCopies_When_ResponseHasMessageIdButNoUID_Then_KeepsTheCopy` and `Test_FetchMessageCopies_When_UIDlessResponsesShareMessageId_Then_CountsEachCopy`);
  - `StreamMessagesByUIDs` for a FETCH response with a message number and no item after the word `FETCH` (`* 7 FETCH`) (`Test_StreamMessagesByUIDs_When_FetchResponseHasNoItemList_Then_OnlyItsBatchFails`). Only the batch of that response fails, and the client does not reconnect. The stream fetches the next batch. Without the length check in `bodyFetchHandler`, the go-imap reader goroutine panics;
  - a very large message count from EXAMINE or STATUS, which must not cause a large allocation (`Test_FetchMessageMap_When_ServerReportsHugeMessageCount_Then_AllocationStaysSmall`);
  - `withCancel` after stop, and shared limiters that draw from one budget.

  The Message-Id scan has these tests:
  - `Test_FetchMessageMap_When_MessagesShareMessageId_Then_KeepsEveryUIDInAscendingOrder` - the map value of one Message-Id lists each copy with its UID and size in ascending UID order. A message with no Message-Id gives one warning;
  - `Test_FetchMessageMap_When_ServerRepeatsAResponse_Then_KeepsTheUIDOnce` - a repeated response with the same UID counts one time, also inside a group of copies;
  - `Test_FetchMessageMap_When_ConnectionDropsMidScan_Then_RetryKeepsEachMessageOnce` - after a cut scan, the retry keeps each message one time and adds no second warning. The UIDVALIDITY is the value of the new session;
  - `Test_FetchMessageMap_When_ResponseHasNeitherMessageIdNorUID_Then_ScanSucceeds` - a response with no Message-Id and no UID does not fail the scan;
  - `Test_FetchMessageMap_When_UIDlessScanIsCutAndRetryIsClean_Then_Succeeds` - a dropped connection comes before the missing-UID error. The client reconnects, and the clean retry succeeds.

  The UIDVALIDITY check has these tests:
  - `TestCheckUIDValidity` - the check passes when the plan holds 0 or the two values are equal. It fails for a changed value, and for a session that sends none (`now none`, not `now 0`);
  - `Test_FetchMessageMap_When_ExamineReportsUIDValidity_Then_ReturnsIt` - the second result is the value of the EXAMINE, or 0 when the server sends none;
  - `Test_FetchMessageMap_When_FolderStaysSelected_Then_StatusReportsUIDValidity` - when the folder stays selected, the scan sends no EXAMINE and reads the value from `STATUS (MESSAGES UIDVALIDITY)`;
  - `Test_FetchMessageMap_When_RetriedOnANewSession_Then_ReturnsTheNewSessionValue` - after a reconnect, the result holds the value of the new session;
  - `Test_StreamMessagesByUIDs_When_UIDValidityDiffers_Then_FetchesNothing` - a session with another value gets no UID FETCH and no reconnect. The error has the class `ClassUnknown`, also when the folder name holds a transient phrase;
  - `Test_StreamMessagesByUIDs_When_KeptSelectionHasOtherUIDValidity_Then_FetchesNothing` - the check also reads the value of a kept selection. A plan value that differs fetches nothing. A plan value that matches fetches the UID, and the stream sends no EXAMINE: the one EXAMINE of the session comes from the scan;
  - `Test_StreamMessagesByUIDs_When_UIDValidityChangesAcrossReconnect_Then_StopsWithoutFetching` - a change after a BYE between two batches stops the stream. The first batch stays delivered, and the new session fetches nothing;
  - `Test_StreamMessagesByUIDs_When_UIDValidityChangesOnRetryInsideFirstBatch_Then_FetchesNoUndeliveredUID` - the retry inside one batch runs the check too, so the stream fetches no UID that it did not deliver;
  - `Test_StreamMessagesByUIDs_When_SessionReportsNoUIDValidity_Then_FetchesNothing` - a session that sends none fails the check when the plan holds a value. This holds after a new EXAMINE and after a kept selection;
  - `Test_StreamMessagesByUIDs_When_PlanHasNoUIDValidityOrValuesMatch_Then_Streams` - a plan value of 0, or a value that matches, streams all UIDs.

  The UID of a delivered message has these tests:
  - `TestMessage_UID` - `Message.UID` gives the UID that the server sent, and 0 for a zero Message or a message with no UID;
  - `Test_StreamMessagesByUIDs_When_MessagesAreDelivered_Then_UIDIsTheRequestedOne` - each delivered message reports one of the requested UIDs, in ascending order.

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
  - `TestIsThrottled` - the predicate is true for a throttle or quota reply under any wrapper, and for a throttled give-up of a reconnect. It is false for a network error, also under a wrapper that holds a quota phrase. It is false for a locked account that names a quota. It is false for the go-imap text `imap: cannot send literal: no continuation request received` and for `ErrUIDValidityChanged`. It is false for a whole `StreamError` with a network error in `First`, so callers read `Stop`;
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

  The session check of the destination has these tests:
  - `Test_EnsureSession_When_SessionIsLive_Then_SendsNothing` - a working session gets no dial and no command;
  - `Test_EnsureSession_When_SessionEndedWhileIdle_Then_ReconnectsOnce` - after a BYE while idle, the call logs in one time, and a later command works;
  - `Test_EnsureSession_When_GateRefuses_Then_ReturnsTheStoredGiveUpWithoutDial` - each call returns the stored give-up with no dial and no LOGIN. This holds inside the cool-down of a transient give-up, and after a permanent give-up, also with `reviveDur` zero;
  - `Test_EnsureSession_When_ReviveIntervalPassed_Then_OneLoginDecides` - after `reviveInterval`, one login attempt runs. A successful attempt gives nil and the full reconnect budget. A failed attempt gives its error, and the next call sends no LOGIN;
  - `Test_EnsureSession_When_GiveUpIsThrottled_Then_IsThrottledReportsIt` - a throttled give-up gives an error for which `IsThrottled` is true, with no dial;
  - `Test_EnsureSession_When_CancelledOrContextEnded_Then_ReturnsWithoutDial` - a cancelled Client gives `context.Canceled`, an ended context gives its error, and neither dials;
  - `Test_EnsureSession_When_ContextEndsDuringReconnect_Then_ReturnsContextError` - a context that ends while the login of a reconnect waits gives the context error, not a stored give-up.

  The halt hook and the stop of a stream have these tests:
  - `Test_StreamMessagesByUIDs_When_HaltBeforeTheFirstBatch_Then_SendsNoCommand` - a halt error comes back unchanged, and the stream sends no EXAMINE and no UID FETCH;
  - `Test_StreamMessagesByUIDs_When_HaltTurnsOnAfterABatch_Then_NoNextBatch` - with 501 UIDs, a halt after the 500th message gives one UID FETCH and 500 deliveries;
  - `Test_StreamMessagesByUIDs_When_HaltTurnsOnDuringARetry_Then_NewSessionGetsNoFetch` - a halt that turns on before the retry of a cut batch sends no UID FETCH on the new session;
  - `Test_StreamMessagesByUIDs_When_CallbackStopsTheStream_Then_SessionClosesAndNothingIsRetried` - an `onMessage` error with UIDs in flight makes the client close the connection. The stream returns the error with one LOGIN and one UID FETCH, and a later stream reconnects and delivers. When the server drops the connection after the error, the stream starts no reconnect and no second UID FETCH;
  - `Test_StreamMessagesByUIDs_When_HaltIsOnAndTheClientHasNoSession_Then_SendsNoLogin` - a halt error comes back before the stream reconnects a Client that has no session, so the stream sends no LOGIN.

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
  - `Test_AppendMessage_When_BodyWasSent_Then_RepairsWithoutRetry` - after go-imap read the body (a one-byte body too), the call repairs the session and sends no second APPEND;
  - `Test_AppendMessage_When_ServerStopsReadingLiteral_Then_WatchdogTripsAndRepairs` - a failure in the middle of the literal gets a repair and no second APPEND;
  - `Test_AppendMessage_When_RevivedSessionFailsBeforeBodyIsRead_Then_DoesNotReconnectAgain`, `Test_AppendMessage_When_RevivedSessionFailsTransiently_Then_DoesNotReconnectAgain` and `Test_AppendMessage_When_RetryGoesSilent_Then_WatchdogTripsWithoutAnotherReconnect` - one call reconnects a maximum of one time;
  - `Test_AppendMessage_When_RetryReconnectIsRefused_Then_ReturnsReconnectError` - a refused login for the retry returns the reconnect error, and the next call returns the same refusal with no new login;
  - `Test_AppendMessage_When_ReconnectBeforeAppendIsRefused_Then_ReturnsReconnectError` - when the server ended the session while idle and refuses each login attempt, `AppendMessage` returns the reconnect error and sends no APPEND;
  - `Test_AppendMessage_When_ContextEndsDuringRetry_Then_ReturnsContextError` - a context that ends while the retry waits returns its error, with no further reconnect;
  - `Test_AppendMessage_When_ContextEndsDuringReconnectBeforeAppend_Then_ReturnsContextError` and `Test_AppendMessage_When_ContextEndsDuringRetryReconnect_Then_ReturnsContextError` - a context that ends while a login waits returns the context error, not the reconnect error, and no APPEND reaches a session;
  - `Test_AppendMessage_When_RetriedAfterEarlyFailure_Then_WireMatchesADirectAppend` - the retry sends the same flags, date and bytes as an APPEND that needed no retry, and exactly one complete literal reaches the server;
  - `Test_readTracker` - `readTracker` marks a body as read only through `Read`, and it does not offer `WriteTo`, so `io.Copy` cannot move the body without a `Read`;
  - `Test_AppendMessage_When_RepairWasRefusedPermanently_Then_NextAppendSendsNoLogin` and `Test_AppendMessage_When_ReconnectWasRefusedPermanently_Then_LaterAppendsSendNoLogin` - after a permanent refusal, `AppendMessage` sends no LOGIN and no APPEND.

  The refusal of an empty or missing body has these tests:
  - `Test_AppendMessage_When_SourceBodyIsEmpty_Then_SendsNoAppend` - a body that go-imap reads as empty (`NIL`, `{0}` or an empty quoted string) gives `ErrEmptyBody` and no APPEND;
  - `Test_AppendMessage_When_SourceBodyIsANonEmptyQuotedString_Then_RefusesAsEmpty` - go-imap keeps no quoted string, so a quoted body that is not empty also reads as empty and gives `ErrEmptyBody`;
  - `Test_AppendMessage_When_BodyIsEmptyAndSessionIsGone_Then_SendsNoDialLoginOrAppend` - the refusal comes before the reconnect, so a Client with no session sends no dial, LOGIN or APPEND. This holds after a give-up, after a BYE and after a hang-up while idle;
  - `Test_AppendMessage_When_BodyHasOneByte_Then_Appends` - a body of one byte passes the guard and reaches the server.

  The flags and the date of a copied message have these tests:
  - `Test_validFlag` - a flag passes only when it is an atom, with at most one backslash at the start. Every single byte, with and without a backslash, passes or fails as the grammar says. A space, a control byte, DEL and a byte above 0x7e fail. A quote, `(`, `)`, `{`, `%`, `*`, `]`, a second backslash and an injected command fail also;
  - `Test_appendFlags`, `Test_appendFlags_When_NothingIsDropped_Then_AllocatesNothing` and `FuzzAppendFlags` - the list keeps the order and the spelling of the keywords and of `\Seen`, `\Answered`, `\Flagged` and `\Draft`. It drops `\Recent`, `\Deleted`, each other system flag and each invalid flag, in any letter case. A list that drops nothing is the same slice, and a list with no flag left is nil. The input never changes, and the checks cost no allocation;
  - `Test_AppendMessage_When_SourceHasFlags_Then_AppendCarriesThemWithoutRecentOrDeleted` and `Test_AppendMessage_When_NoFlagRemains_Then_AppendHasNoFlagList` - the APPEND carries the kept flags in the order of the source, and it has no flag list when no flag stays. The flags of the message do not change;
  - `Test_AppendMessage_When_FlagWouldBreakTheCommandLine_Then_DropsItAndSendsOneCleanAppend` - a flag with a line break and a command, or with brackets, braces or quotes, never reaches the wire. The server sees only LOGIN and one APPEND;
  - `Test_appendDate` and `Test_AppendMessage_When_SourceHasInternalDate_Then_AppendCarriesItNotTheDateHeader` - the INTERNALDATE wins over the `Date` header and keeps its own offset. The `Date` header is the fallback. With neither, the APPEND has no date. The date comes after the flags;
  - `Test_StreamMessagesByUIDs_When_ResponseCarriesFlags_Then_MessageKeepsTheServerSpelling`, `Test_StreamMessagesByUIDs_When_ItemNamesAreLowerCase_Then_FlagsAndDateAreKept` and `Test_StreamMessagesByUIDs_When_ResponseCarriesInternalDate_Then_MessageKeepsIt` - the UID FETCH asks for `FLAGS` and `BODY.PEEK[]`. The message keeps the spelling of each flag, also when the item names are in lower case. The date reads with a padded day, with a day that has no padding and with a positive or a negative offset. A missing date, a NIL date and text that is not a date leave the date at zero;
  - `Test_StreamMessagesByUIDs_When_UnilateralOrUnrequestedFetchArrives_Then_CallbackSkipsIt` and `Test_bodyFetchHandler_Handle` - a response with no UID, with UID 0 or with a UID that nobody asked for goes to go-imap as unilateral data. The handler ignores a response that is not a FETCH. A response with a bad message number, with no item list or with an item list that is not a list fails that batch. The flags keep the spelling of the server. The last `FLAGS` item wins. A literal item keeps the text that go-imap parsed;
  - `Test_uidFetchBodies_When_SessionHasNoSelection_Then_ReturnsErrNoMailboxSelectedAndClosesTheChannel` - without a selected folder, the function sends no command, returns `ErrNoMailboxSelected` (class `ClassTransient`) and closes the channel.

  The body stream and its failed batches have these tests:
  - `Test_StreamMessagesByUIDs_When_CallbackBlocks_Then_AtMostOneBodyIsReadAhead` - while the callback holds one body, go-imap reads one more and waits. The test runs on `net.Pipe`, which has no buffer, and counts the responses that the client read. The callback of message k sees at most k+1 responses, so a worker holds at most two bodies;
  - `Test_StreamMessagesByUIDs_When_UIDOnlyGetsBodylessResponses_Then_CallbackSkipsItAndStreamSucceeds` - a UID that gets only responses without a body never reaches the callback. The stream succeeds and sends no second UID FETCH;
  - `TestStreamError` - the message shows the first failure and, for two or more failed batches, the count `(N batches failed)`. `Unwrap` lists the stop cause before the first failure. A throttled stop beats an earlier permanent failure, and without a stop the first failure decides the class;
  - `Test_StreamMessagesByUIDs_When_ABatchIsRefused_Then_LaterBatchesAreStillFetched` - a refused batch costs only itself. The stream fetches the next batches and fetches no UID two times. The error is a `*StreamError` with one failed batch and no stop, and the stream uses one LOGIN and one EXAMINE;
  - `Test_StreamMessagesByUIDs_When_SeveralBatchesFail_Then_ErrorKeepsTheFirstAndCountsThem` - the error keeps the first failure and counts the failed batches. This holds when the server refuses every batch. It also holds when the server refuses only the first and the last batch;
  - `Test_StreamMessagesByUIDs_When_ABatchFailsAfterItsRetry_Then_TheNextBatchRunsOnANewSession` and `Test_StreamMessagesByUIDs_When_ExamineIsCutOnBothSessions_Then_ItIsNoRefusalAndTheNextBatchRuns` - a batch that two sessions cut fails with a transient first failure. A dropped session is no refusal, so the client does not mark it with `ErrSelectRefused`. The next batch runs on a third session;
  - `Test_StreamMessagesByUIDs_When_AStopCauseArrives_Then_TheStreamEndsThere` - a throttled reply, also after an unknown refusal, and a changed UIDVALIDITY end the stream at once and fill `Stop`. A changed UIDVALIDITY sends no UID FETCH;
  - `Test_StreamMessagesByUIDs_When_ExamineIsRefused_Then_StreamEndsAfterOneExamine` and `Test_StreamMessagesByUIDs_When_FolderIsRefusedAfterTheFirstBatch_Then_LaterBatchesAreNotRequested` - a refused EXAMINE marks `ErrSelectRefused` and ends the stream after one EXAMINE. This holds for the first batch and for a later batch, and a folder name with a throttle phrase does not change the class;
  - `Test_StreamMessagesByUIDs_When_ExamineGetsAThrottledReply_Then_TheStreamEndsThere` - a throttled reply to EXAMINE ends the stream with one EXAMINE and no UID FETCH. It does not mark `ErrSelectRefused`, because a throttle can clear;
  - `Test_StreamMessagesByUIDs_When_UIDValidityChangesAfterARefusedBatch_Then_TheStreamStops` - a changed UIDVALIDITY on the new session ends the stream. `First` holds the refusal of the first batch and `Stop` holds the mismatch;
  - `Test_StreamMessagesByUIDs_When_ClientHasGivenUp_Then_EachBatchFailsWithoutALogin` - a Client that gave up (a permanent refusal, or transient refusals inside the revive interval) fails each batch with the stored give-up. The error counts three failed batches, and the stream sends no dial, LOGIN or UID FETCH;
  - `Test_StreamMessagesByUIDs_When_CancelledAfterARefusedBatch_Then_ReturnsCanceledAndFetchesNoMore` - an ended context and a cancelled Client return the cancel error and fetch no more.

  The existence check has these tests:
  - `Test_ExistingUIDs_When_ServerAnswers_Then_ResultIsTheHeldSubsetOfTheInput` and `Test_ExistingUIDs_When_ReplyNamesUIDsBetweenTheAskedOnes_Then_OnlyTheAskedUIDsCount` - the result is the ascending subset of the asked UIDs that the folder holds. A UID that nobody asked for, or UID 0, in the reply never stands for a neighbour;
  - `Test_ExistingUIDs_When_NothingIsAsked_Then_SendsNoCommand` and `Test_ExistingUIDs_When_InputRepeatsEachUID_Then_OneCommandCarriesTheDistinctUIDs` - an empty request sends no command, and a repeated UID goes out one time;
  - `Test_ExistingUIDs_When_MoreThanOneBatchIsAsked_Then_EachCommandCarriesAtMost500UIDs` and `Test_ExistingUIDs_When_SecondBatchFails_Then_ReturnsNoUIDsAndTheError` - a request above `uidFetchBatchSize` splits into more than one command, and a failed batch returns no UID and the error;
  - `Test_ExistingUIDs_When_FolderIsAlreadySelected_Then_SendsNoSecondExamine` and `Test_ExistingUIDs_When_UIDValidityDiffers_Then_SendsNoSearch` - the check uses `selectIfNeeded`, and it checks the UIDVALIDITY before the search;
  - `Test_ExistingUIDs_When_ServerRefusesTheSearch_Then_ReturnsTheLabeledErrorWithoutReconnect` - a refusal returns the error `[label] check UIDs in FOLDER: ...` with one LOGIN and one UID SEARCH;
  - `Test_ExistingUIDs_When_SessionEndsDuringTheSearch_Then_ReconnectsSelectsAgainAndSearchesAgain` - after a dropped connection or a BYE, the call logs in, selects the folder and searches again;
  - `Test_ExistingUIDs_When_ContextEnded_Then_ReturnsItsErrorAndSendsNoCommand`, `Test_ExistingUIDs_When_ContextEndsDuringTheSearch_Then_ReturnsItsError` and `Test_ExistingUIDs_When_ContextEndsAfterAReply_Then_ReturnsItsErrorAndSendsNoMoreSearch` - a context that ends gives its error at each of these points and sends no more command;
  - `Test_uidSearchIDs_When_SessionHasNoSelection_Then_ReturnsErrNoMailboxSelectedAndSendsNothing` - without a selected folder, the search sends no command.

  The capability read has these tests:
  - `Test_New_When_ServerAdvertisesGmailExtensionAfterLogin_Then_KeepsOneCopyPerMessageID` - when the CAPABILITY reply lists `X-GM-EXT-1`, `KeepsOneCopyPerMessageID` is true and `New` sends one CAPABILITY. This holds also when LOGIN OK carried a list;
  - `Test_New_When_ServerSpellsTheGmailExtensionInAnotherLetterCase_Then_KeepsOneCopyPerMessageID` - the spellings `x-gm-ext-1` and `X-Gm-Ext-1` count as the capability, in the CAPABILITY reply and in LOGIN OK;
  - `Test_New_When_ServerDoesNotAdvertiseGmailExtensionAfterLogin_Then_ReportsFalse` - the flag is false when the capability is in no list, only in the greeting, or only in LOGIN OK. The CAPABILITY reply after LOGIN decides;
  - `Test_New_When_UntaggedCapabilityComesBeforeLoginOK_Then_TheExplicitReplyDecides` - go-imap drops a capability list that comes before LOGIN OK. So the CAPABILITY reply that `New` asks for decides, and `New` sends one CAPABILITY;
  - `Test_New_When_GreetingHasNoCapabilityList_Then_OneProbeComesBeforeLoginAndOneCheckAfterIt` - when the greeting has no list, go-imap sends one CAPABILITY before LOGIN, and `New` sends one after LOGIN. The server sees two;
  - `Test_New_When_LoginUsesCramMD5_Then_TheCapabilityReplyDecides` - after AUTHENTICATE, the CAPABILITY reply decides, whatever list the AUTHENTICATE OK carries. `New` sends one AUTHENTICATE, no LOGIN and one CAPABILITY;
  - `Test_New_When_CapabilityIsRefused_Then_FailsAndNamesTheLabel` - a refused CAPABILITY fails `New` with `failed to read server capabilities` and `[label] capability`. `New` returns no client and logs out;
  - `Test_New_When_ConnectionDropsDuringCapabilityCheck_Then_ReconnectsAndReadsTheNewSession` - a dropped connection gets one reconnect, and the CAPABILITY reply of the new session decides;
  - `Test_New_When_ConnectionDropsDuringCapabilityCheckAndReloginIsRefusedPermanently_Then_FailsAndClosesEverySocket` - a dropped connection, and a reconnect that the server refuses with `Authentication failed`, make `New` fail. The error holds `failed to read server capabilities` and has the class `ClassPermanent`. `New` returns no client, and the server sees two LOGIN commands. The fake server keeps each session open until the client hangs up. The test waits for the end of both sessions;
  - `Test_New_When_ConnectionDropsDuringCapabilityCheckAndEveryReloginFailsTemporarily_Then_FailsWithTheLabel` - a dropped connection, and a temporary authentication failure on every reconnect, make `New` fail. The error holds `failed to read server capabilities` and `[dst] failed to reconnect after retries`. The server sees one LOGIN for the first session and one for each reconnect attempt. The test waits for the end of each of these sessions;
  - `Test_New_When_ContextEndsDuringCapabilityCheck_Then_ReturnsCanceled` - a context that ends while CAPABILITY waits returns `context.Canceled` and no client, and `New` sends no second LOGIN;
  - `Test_detectOneCopyPerID_When_ContextAlreadyEnded_Then_SendsNoCommand` - a context that ended before the call gives the context error and sends no CAPABILITY;
  - `Test_detectOneCopyPerID_When_ContextEndsAfterTheReply_Then_ReturnsErrorAndStoresNothing` - a context that ends after the reply gives the context error, and the flag stays false;
  - `Test_detectOneCopyPerID_When_ServerStaysSilentOnCapability_Then_WatchdogTripsAndTheRetryReadsTheNewSession` - a server that does not answer CAPABILITY makes the watchdog close the session. The check then reconnects and reads the CAPABILITY reply of the new session. The server sees two LOGIN and two CAPABILITY commands;
  - `Test_Client_When_SessionIsReplacedAfterTheCheck_Then_KeepsTheValueAndAsksNothingMore` - after `New`, a BYE in reply to EXAMINE makes `FetchMessageMap` reconnect and try again. The new session does not advertise `X-GM-EXT-1`, but the flag keeps the value of `New`, and the reconnect sends no CAPABILITY;
  - `Test_hasCapability` - the name matches in any letter case among other names. A nil map, an empty map, a longer, shorter or other name, a look-alike letter and a false value do not match;
  - `Test_hasCapability_When_Called_Then_AllocatesNothing` - a match and a miss cost no allocation.

  The greeting and the rate limit have these tests:
  - `Test_New_When_SharedLimiterIsInDebt_Then_GreetingDoesNotWaitAndLoginDoes` - a debt on a shared limiter does not count against `dialTimeout`. Three cases run. First: a write limiter, and a greeting with no capability list. Second: a read limiter, and a greeting with a list. Third: a read limiter, and a greeting with no list. `New` succeeds. It takes at least 1 s, because LOGIN waits for the debt. The server sees one LOGIN. It sees two CAPABILITY commands when the greeting has no list, and one when the greeting has a list;
  - `Test_AppendMessage_When_SessionDropsWhileSharedWriteLimiterIsInDebt_Then_ReconnectsAndAppends` - a session that ends while the shared write limiter is in debt gets a reconnect with a greeting that has no capability list. The debt is longer than all reconnect attempts together. `AppendMessage` returns nil, and the server sees two LOGIN commands and one APPEND. The Client holds no give-up. The call takes at least half of the debt;
  - `Test_New_When_SharedLimiterIsInDebtAndAuthIsCramMD5_Then_GreetingDoesNotWaitAndAuthenticateDoes` - the same check for CRAM-MD5, with a write limiter and with a read limiter. The greeting does not wait for the debt, and AUTHENTICATE waits;
  - `Test_connectAndLogin_When_ReadLimiterIsSet_Then_TheGreetingTakesTokensWithoutWaiting` - the limiter is in a debt of about 131000 tokens. The greeting takes its tokens, so the shared budget counts it. The greeting does not wait. The test stops at LOGIN;
  - `Test_New_When_ServerNeverGreetsAndLimitersAreSet_Then_FailsAndClosesTheSocket` - with both limiters set, a server that never greets makes `New` fail with `os.ErrDeadlineExceeded` after `dialTimeout`. The client closes the socket and sends no LOGIN.
- **`internal/ratelimit`** - token starvation, nil-limiter pass-through, read+write directions independent.
  The mode with no wait has these tests:
  - `TestConn_When_WaitIsOff_Then_ReadAndWriteTakeTokensWithoutWaiting` - on a limiter in debt, a `Write` and a `Read` return at once with no error. Each direction loses exactly the bytes that moved;
  - `TestConn_When_WaitIsOff_Then_ChargeSplitsAtTheBurst` - a `Write` of 3 bursts and 1 byte returns at once and charges all of the bytes;
  - `TestConn_When_WaitTurnsBackOn_Then_WritesWaitAgain` - after `SetWait(false)` and `SetWait(true)`, a `Write` waits for tokens;
  - `TestConn_When_WaitTurnsOnDuringARead_Then_ThatReadWaits` - a `Read` that began before `SetWait(true)` waits for tokens after the inner read returns, and `Close` ends the wait with `context.Canceled`;
  - `TestConn_When_LimitersAreNilAndWaitIsOff_Then_PassesThrough` - both directions pass through when the limiters are `nil`;
  - `Test_chargeN_When_BurstIsZero_Then_Returns` - a limiter with burst 0 does not cause an endless loop.
- **`internal/utils`** - `AskConfirm` and its helpers. The tests cover the `-y/--confirm` flow. `AskConfirm` reads each answer from one shared buffered reader.
- **`internal/app`** - sync plan diff (count of copies for each Message-Id), subfolder expansion, delimiter reconciliation, worker pool dispatch.
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
  - a destination that sends no UID in the Message-Id scan, in the planning scan and in the check after CREATE, where the run copies only the missing messages and a second run copies nothing (`TestRunSync_When_DestinationScanSendsNoUID_Then_CopiesOnlyTheMissingMessage`, `TestRunSync_When_DestinationScanSendsNoUIDForSharedMessageIdCopies_Then_CopiesOnlyTheMissingCopy` and `TestRunSync_When_ExistingDestinationScanSendsNoUID_Then_TheRecheckDropsTheHeldMessage`);
  - a source that sends no UID in the Message-Id scan, where the folder fails and the other folders copy (`TestRunSync_When_SourceScanSendsNoUID_Then_FolderFailsAndOtherFoldersCopy`);
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
  - `Test_destinationDiff` - the diff of a destination with one plan never adds a UID. It drops the UIDs that the destination holds or the source lost. It scales the size, and it leaves the input slice unchanged. The test helper `rediffOnePlan` runs `destinationDiff` on one plan. For each case, `held` and `gone` have the expected values, and `held`, `gone` and the kept UIDs add up to the planned UIDs;
  - `Test_destinationDiff_When_ScanListsOneUIDUnderTwoMessageIds_Then_EachPlannedUIDCountsOnce` - a scan that lists one UID under two Message-Ids counts that planned UID one time. If the destination holds only one of the two Message-Ids, the plan keeps the UID and `held` and `gone` are 0. If the destination holds both, `held` is 1 and `gone` is 0;
  - `Test_recheckExistingDestinations_When_PlansShareADestination_Then_TheyAreComparedTogether` - the exact output and the kept plans for plans that share a destination. In the first case, the destination holds the copy that matches the second plan. The second case is the same case with `--quiet`, and it gives no output. The third case has a planned message that is gone next to a held one. The fourth case has the plans of two destinations in interleaved order. The lines and the kept plans follow the plan order. The input `SrcUIDs` slices do not change;
  - `Test_recheckExistingDestinations_When_PlansShareADestination_Then_TheDestinationIsReadOnce` - a group of two plans sends one Message-Id FETCH and one EXAMINE to the destination, and one EXAMINE for each source folder;
  - `Test_recheckExistingDestinations_When_OneSharedPlanCannotBeRescanned_Then_OnlyItFails` - a refused EXAMINE of the source folder (`errSourceScan`) and a changed UIDVALIDITY (`client.ErrUIDValidityChanged`) each give one folder failure for that plan only. The other plan is still checked;
  - `Test_recheckExistingDestinations_When_SharedDestinationCannotBeRead_Then_EveryPlanOfItFails` - every plan of that destination fails with `errDestinationRecheck`, and the run sends no source command for them. The run checks the plan of another destination as usual;
  - `Test_recheckExistingDestinations_When_SharedDestinationIsEmpty_Then_NoSourceIsScanned` - the plans keep their UIDs and get `DestinationFolderExists`, and the source gets no EXAMINE, STATUS or FETCH;
  - `Test_recheckExistingDestinations_When_ContextEndsBetweenSharedSourceScans_Then_ReturnsCanceledWithoutOutput` - a context that ends with the second source EXAMINE gives `context.Canceled`, no kept plan, no failure and no output;
  - `Test_destinationDiff_When_UIDsOfTwoSourcesCollide_Then_EachPlanKeepsOnlyItsOwnUIDs` - two source folders with the same UID give the copies to the right plans, because the diff names a copy by UID and slot;
  - `Test_destinationDiff_When_PlansShareADestinationAndSourcesAreUnchanged_Then_TheDestinationEndsWithTheLargestCount` - for random plans of one destination, built with `maybeDiff` for both copy rules, the kept UIDs are planned UIDs. For each Message-Id, the destination copies and the kept copies add up to the largest count of the destination and of the sources. `gone` is 0, and `NewMessages`, `NewSize`, `ExtraCopies` and `RepeatedCopies` follow the kept UIDs;
  - `Test_destinationDiff_When_SourcesChangeAfterThePlan_Then_ResultNeverAddsAndNeverExceedsTheLargestFreshCount` - for random sources that lose copies and gain copies after the plan, the kept UIDs are planned UIDs. `gone` counts the planned UIDs that the new scan does not list, and the count of copies stays at or under the largest count;
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

  The count of copies has these tests:
  - `Test_maybeDiff_When_MessagesShareMessageId_Then_PlansByCount` - the plan copies max(N - D, 0) source copies for each Message-Id. `NewSize` and `ExtraCopies` cover the planned copies only;
  - `Test_maybeDiff_When_SourceAndDestinationCountsVary_Then_PlanAndRecheckPickTheSameMessages` - for each pair of counts, and for equal and different sizes, `maybeDiff` and `destinationDiff` choose the same UIDs;
  - `Test_copyMatcher_match` - a destination copy matches a source copy with the same size. A copy that no size matches takes the lowest free UID;
  - `Test_copyMatcher_When_GroupIsUncoveredOrFullyCovered_Then_AllocatesNothing` - a group that the destination does not touch, or covers in full, costs no allocation;
  - `Test_copyMatcher_When_InputsAreRandom_Then_MatchesTheReference` - random inputs give the same result as a slow reference, and no destination copy matches two source copies;
  - `Test_destinationDiff_When_CountsAndPlanVary_Then_ResultIsASubsetOfThePlan` - for random counts and plans, the recheck never adds a UID, and `NewMessages`, `NewSize`, `held` and `gone` follow the kept UIDs;
  - `TestRunSync_When_SourceMessagesShareMessageId_Then_EachIsCopiedOnce` - each of three messages with one Message-Id arrives one time, and a second run copies nothing;
  - `TestRunSync_When_DestinationHoldsCopiesOfASharedMessageId_Then_CopiesOnlyTheRest` - a destination that holds one, two, three or four copies gets only the copies that it misses;
  - `TestRunSync_When_CreateReportsExistingFolderHoldingASharedMessageId_Then_CopiesTheRest` - the check after CREATE keeps only the copies that the folder misses;
  - `TestRunSync_When_DestinationHoldsOnlyTheHighestUIDCopy_Then_CopiesTheOtherCopiesOnce` and `TestRunSync_When_DestinationMissesAMiddleCopy_Then_CopiesOnlyThatCopy` - the sizes decide which source copies the destination holds, and a second run copies nothing;
  - `TestRunSync_When_ExistingFolderHoldsOnlyTheHighestUIDCopy_Then_TheRecheckCopiesTheOtherCopies` - the check after CREATE matches by size in the same way;
  - `TestRunSync_When_FolderHasSkippedAndExistingMessages_Then_PreviewSizeCountsOnlyPlannedMessages` - the preview size adds up only the planned copies. It does not count the copies that the destination holds, or the messages with no Message-Id;
  - `TestRunSync_When_NoSelectDestinationReceivesDuplicateMessageIds_Then_FailureCountsEveryMessage` - the failure of a `\Noselect` destination counts each source message, also the messages that share a Message-Id.

  The note of extra copies has these tests:
  - `TestRunSync_When_DestinationKeepsOneCopyPerMessageId_Then_EveryRunNotesTheExtraCopies` - a destination that keeps one message for each Message-Id but does not advertise X-GM-EXT-1 gets the note on each later run. The first run prints none. The fake server has `keepOnePerMessageID` and no `advertiseGmailExtension`;
  - `TestRunSync_When_DestinationHoldsOneOfThreeCopies_Then_QuietRunPrintsNoNote` - with `--quiet`, the run prints no note and no preview;
  - `TestRunSync_When_VerbosePreviewHasCopiesTheDestinationHolds_Then_NoteComesBeforeTheUIDList` - with `--verbose`, the note comes after the "will copy" line and before the UID list.

  The one-copy rule has these tests:
  - `Test_copyRule_sourceCopies` - the count rule keeps each group. The one-copy rule cuts a group to its first copy, which has the lowest UID. Neither call allocates;
  - `Test_copyRule_When_ZeroValue_Then_CopiesByCount` - the zero value of `copyRule` is the count rule, and a zero plan holds this rule. So a plan without a rule keeps every copy;
  - `Test_maybeDiff_When_DestinationKeepsOneCopyPerMessageID_Then_PlansTheLowestUIDOnce` - the plan holds the lowest UID of each Message-Id, and none when the destination holds the Message-Id. `RepeatedCopies` counts the cut copies, `ExtraCopies` stays 0, the plan stores the rule, and `NewSize` and the totals count the planned copies only;
  - `Test_maybeDiff_When_DestinationCountsByCopies_Then_NothingIsLeftOut` - the count rule plans every copy, and `RepeatedCopies` stays 0;
  - `Test_maybeDiff_When_OneCopyDestinationHoldsTheIdAtTheFirstSlot_Then_NothingIsReserved` - when the destination holds the Message-Id, the first of two mappings with one destination reserves no copy, because its plan copies nothing;
  - `Test_maybeDiff_When_OneCopyDestinationIsShared_Then_LaterMappingsGetStandbysNotPlans` - on a shared destination, the first mapping that lists the Message-Id plans one copy. The later mappings plan none, and their copies become standbys. The last mapping releases the reservations;
  - `Test_maybeDiff_When_DestinationKeepsOneCopyAndCountsVary_Then_PlanAndRecheckPickTheSameMessages` - for each pair of source and destination counts, `maybeDiff` and `destinationDiff` choose the same UID, the same size and the same `RepeatedCopies`;
  - `Test_destinationDiff_When_PlanKeepsOneCopyPerMessageID_Then_TheRecheckAppliesTheSameRule` - the check after CREATE reads `plan.copyRule`. It sets `NewSize`, `RepeatedCopies` and `ExtraCopies` again, counts `held` and `gone`, and leaves the input slice unchanged;
  - `Test_destinationDiff_When_OneCopyRuleAndPlanVary_Then_ResultIsASubsetOfThePlan` - for random counts and plans, the check never adds a UID and keeps at most one UID of each Message-Id. `NewSize`, `RepeatedCopies`, `held` and `gone` follow the kept UIDs;
  - `Test_destinationDiff_When_PlanHasNoCopyRule_Then_CountsByCopiesAndLeavesNothingOut` - a plan without a rule gets the count rule in the check after CREATE. `RepeatedCopies` becomes 0, and `ExtraCopies` counts the kept copies whose Message-Id the destination holds. `held` counts the dropped copies;
  - `Test_recheckExistingDestinations_When_OneCopyPlanLosesAHeldId_Then_KeptPlanCountsOnlyItsOwnRepeatedCopies` - the check after CREATE drops the planned copy of a Message-Id that the destination holds. The kept plan counts the cut copies of the kept Message-Ids only, and the check prints the "already exists and holds" line;
  - `TestRunSync_When_GmailDestinationGetsRepeatedMessageIds_Then_OneCopyArrivesAndTheNextRunCopiesNothing` - three source copies of one Message-Id give a plan of one message and the repeated-copies line with 2. The copy with the lowest UID arrives, the preview has no extra-copies line, and a second run copies nothing;
  - `TestRunSync_When_GmailFolderHoldsRepeatedAndSingleMessageIds_Then_OnlyTheLowestCopyOfEachIdIsPlanned` - in one folder with three Message-Ids, the plan holds the copy with the lowest UID of each. The repeated-copies line shows 3. The lowest copy is not always the smallest, so a pick by size fails the test. A second run copies nothing;
  - `TestRunSync_When_GmailDestinationHoldsTheMessageId_Then_NoCopyOfItIsPlanned` - a destination that holds the Message-Id gets none of its copies, only the other messages. The preview shows neither note;
  - `TestRunSync_When_GmailDestinationHoldsOneOfTwoMessageIds_Then_OnlyTheOtherIdIsPlanned` - the destination holds one of two Message-Ids. The plan holds the lowest copy of the other one, and the repeated-copies line shows 1. This holds when the destination scan sends the UID of the held copy and when it sends none. A second run copies nothing;
  - `TestRunSync_When_MappingsGoToDifferentGmailFolders_Then_EachFolderGetsItsOwnCopy` - two mappings go to two different destination folders. Each mapping plans one copy of a shared Message-Id. Only the source folder with a repeated copy gets a repeated-copies line. This holds with one worker and with two workers;
  - `TestRunSync_When_TwoMappingsShareAGmailDestination_Then_TheSharedMessageArrivesOnce` - the shared message arrives one time, with one worker and with two workers. This holds also when every copy in the folder of the first mapping has an empty body. Then the copy of the second mapping arrives. A second run copies nothing;
  - `TestRunSync_When_ThreeMappingsShareAGmailDestination_Then_OneCopyArrivesAndTheNextRunCopiesNothing` - of three mappings with one destination, only the first plans a copy. This holds with one worker and with two workers when the destination is missing. The case where the destination already holds the Message-Id runs with two workers, and the run plans nothing. A second run copies nothing;
  - `TestRunSync_When_HiddenGmailDestinationHoldsTheMessageId_Then_TheRecheckAppliesTheOneCopyRule` - for a folder that CREATE reports as existing, the check after CREATE drops the planned copy whose Message-Id the folder holds. This holds also when the held copy has the size of another source copy. The run prints the "already exists and holds" line;
  - `TestRunSync_When_HiddenGmailDestinationHoldsOneOfTwoMessageIds_Then_OnlyTheOtherIdIsCopied` - the check after CREATE drops the held Message-Id and keeps the other one. The held copy has the size of the third source copy. The preview shows 3 repeated copies, and the run prints the "already exists and holds" line;
  - `TestRunSync_When_PlannedLowestGmailCopyLeavesTheSourceBeforeTheRecheck_Then_TheHigherCopyIsNotAddedUntilTheNextRun` - a planned copy leaves the source after the plan. The check after CREATE drops it and adds no higher copy of the same Message-Id. The run prints the "already exists and holds" line and the "no longer holds" line, sends no UID FETCH and opens no worker connection. The next run copies the higher copy;
  - `TestRunSync_When_MirrorRunHasAGmailDestination_Then_EveryFolderAndSubfolderGetsOneCopyPerMessageId` - in a mirror run, each folder and each subfolder gets one copy of each Message-Id. Neither server gets an EXAMINE for the `\Noselect` folder `[Gmail]`. A second run copies nothing;
  - `TestRunSync_When_GmailDestinationRunsQuiet_Then_PrintsNoRepeatedCopiesLine` - with `--quiet`, the run prints no preview and no repeated-copies line;
  - `TestRunSync_When_VerbosePreviewHasRepeatedCopies_Then_LineComesBeforeTheUIDList` - with `--verbose`, the repeated-copies line comes after the "will copy" line and before the UID list;
  - `TestRunSync_When_OnlyTheSourceAdvertisesGmailExtension_Then_CopyByCountStays` - a source that advertises the capability does not change the plan. All three copies are planned, and the preview has no repeated-copies line;
  - `TestRunSync_When_DestinationAdvertisesGmailExtension_Then_VerboseRunNamesTheOneCopyRule` - with `--verbose`, the run prints the rule line one time, after the server delimiters and before the next section. This holds also when the destination already holds the Message-Id. The run prints no line without `--verbose`, with `--verbose` and `--quiet` together, or when only the source advertises the capability.

  The capability read in `runSync` and `runShow` has these tests:
  - `Test_copyRuleOf_When_DestinationAdvertisesGmailExtension_Then_ChoosesTheOneCopyRule` - a Client for a server that advertises `X-GM-EXT-1` gives the one-copy rule. A Client for a server that does not gives the count rule;
  - `TestRunSync_When_GmailRunUsesTwoWorkers_Then_EachConnectionAsksForCapabilitiesOnce` - with two workers, each server sees three LOGIN commands and three CAPABILITY commands. The three connections are the planning Client and the two workers, so the run sends no CAPABILITY for each plan;
  - `TestRunSync_When_ServerRefusesCapability_Then_RunFailsBeforeAnyScanAndNamesTheLabel` - when the source or the destination refuses CAPABILITY, `runSync` fails. The error holds `failed to read server capabilities`, `capability refused` and `[src] capability` or `[dst] capability`. The run prints no preview, and neither server gets EXAMINE, STATUS, FETCH, CREATE or APPEND;
  - `TestRunShow_When_ServerRefusesCapability_Then_NamesTheFailingAccount` - when the source or the destination refuses CAPABILITY, `runShow` fails. The error holds `failed to read server capabilities`, `capability refused` and the label of the failing account, for example `[alpha] connect` and `[alpha] capability` when the source refuses. It does not show the password.

  The shared destinations have these tests:
  - `Test_maybeDiff_When_PlansShareADestination_Then_LaterPlansCopyOnlyTheExcess` - a later plan copies only the copies that the earlier plans and the destination do not cover;
  - `Test_maybeDiff_When_ThreeMappingsShareADestinationAmongOthers_Then_CountsAccumulateAndAreReleased` - the reservations of three mappings add up, a destination with one mapping reserves nothing, and the last mapping releases the reservations;
  - `Test_maybeDiff_When_SharedCopiesDifferInSize_Then_SizesPairReservationsAndStandbys` - the sizes pair the copies of a later mapping with the reserved copies, and they choose the standbys;
  - `Test_buildSyncPlan_When_SharedDestinationScansFinishInAnyOrder_Then_PlansAreIdentical` - a slow source scan or a slow destination scan changes neither the plans nor the standbys;
  - `Test_lastIndex_When_ReceiverIsNil_Then_ReportsNoSharedDestination` - a run with no shared destination holds no set;
  - `TestRunSync_When_TwoMappingsShareADestination_Then_ASharedMessageIdIsCopiedOnce` - a message that two source folders share arrives one time. This holds for one and for two workers, and for an existing and a missing destination;
  - `TestRunSync_When_SharedDestinationHoldsCopies_Then_TheLargestCountWins` - the destination ends with the largest count of copies, not the sum, and a second run copies nothing;
  - `TestRunSync_When_SharedDestinationMapsInAnotherOrder_Then_TheFirstMappingCopiesTheSharedMessage` - the first mapping in the list copies the shared message;
  - `TestRunSync_When_TheFirstSharedMappingFails_Then_TheSecondCopiesTheSharedMessage` - a failed scan of the first mapping gives a folder failure, and the second mapping copies the shared message;
  - `TestRunSync_When_TheLastSharedMappingFailsItsScan_Then_EarlierPlansStillCopy` - a failed scan of the last mapping releases the reservations, and the earlier plans still copy;
  - `TestRunSync_When_TheFirstSharedMappingFailsAtCopyTime_Then_TheStandbyCopiesTheSharedMessageAndTheNextRunCopiesTheRest` - the source folder of the first mapping changes its UIDVALIDITY after the plan. The copy of the second mapping replaces the shared message in the same run. The first mapping counts one error. The next run copies the rest;
  - `TestRunSync_When_AnEarlierMappingCoversALaterOne_Then_TheLaterOneHasNoPlan` - a later mapping whose messages are all covered gets no plan;
  - `TestRunSync_When_ExpandedSubfolderAndMappingShareADestination_Then_EachMessageIsCopiedOnce` - an expanded subfolder and a mapping with one destination copy each message one time;
  - `TestRunSync_When_MappingsNameOneDestinationInTwoLetterCasesOfInbox_Then_TheyShareTheFolder` - two letter cases of an INBOX level in the destination names give one folder;
  - `TestRunSync_When_SharedDestinationExistsButLISTHidesIt_Then_TheRecheckKeepsEachMessageOnce` - the check after CREATE keeps each message one time. This holds when the folder holds a message of the first plan, and when it holds a message of the second plan;
  - `TestRunSync_When_HiddenSharedDestinationHoldsOneSizeOfAMessageId_Then_ItEndsWithEverySize` - two mappings share a destination that holds one copy of a Message-Id. The destination is a folder that LIST hides, an INBOX that LIST hides, or a folder that LIST shows. The folder ends with both sizes and one APPEND, and the first run prints one "holds" line, for the second mapping. Two more runs copy nothing;
  - `TestRunSync_When_ThreeMappingsShareAHiddenDestination_Then_ItEndsWithTheLargestCount` - three mappings share a hidden destination, with one worker and with two workers. The run sends two APPEND commands, prints one "holds" line, for the second mapping, and the folder ends with three sizes. A second run copies nothing;
  - `TestRunSync_When_HiddenSharedDestinationKeepsTheFirstMappingsCopyAndItsBodyIsEmpty_Then_ItsStandbyArrives` - the plan that stays after the check keeps its standbys, so the copy of the second mapping replaces a first copy with an empty body;
  - `TestRunSync_When_SharedDestinationCopiesDifferInSize_Then_EachDistinctCopyArrivesOnce` - copies of one Message-Id that differ in size arrive one time each;
  - `TestRunSync_When_SharedDestinationNameCarriesControlCharacters_Then_PreviewListsItOnceEscapedAndServerGetsOneExactCreate` - the list of folders to create names the destination one time, with escapes. The server gets one CREATE with the exact name.

  The standbys and the refused bodies have these tests:
  - `TestFolderSyncPlan_standbysOf` and `TestFolderSyncPlan_standbysOf_When_Called_Then_DoesNotAllocate` - the standbys of one planned UID come in source order, and the lookup costs no allocation;
  - `Test_standbysFor_When_ReceiverIsNilOrPlanHasNoStandbys_Then_ReturnsNothing` - a nil set, or a slot with no standby, gives nothing;
  - `Test_maybeDiff_When_SharedDestinationHasStandbyCopies_Then_EachPlannedCopyGetsThemInMapOrder` and `Test_maybeDiff_When_SharedDestinationCountsVary_Then_StandbysFollowTheReservations` - each planned copy gets the copies of later mappings as standbys, in mapping order, also for random counts;
  - `Test_buildSyncPlan_When_DestinationIsNotSharedOrSharesNoMessageId_Then_PlansHaveNoStandbys` - a plan has no standby when its destination is not shared, or when the mappings share no Message-Id;
  - `Test_runFolderSync_When_PlanHasNoStandbys_Then_EmptyBodyCountsOneError` - without standbys, a refused body counts one error and the destination gets no APPEND;
  - `Test_runFolderSync_When_PlannedMessageHasEmptyBodyAndAStandby_Then_CopiesTheStandbyOnce` and `Test_runFolderSync_When_PlannedBodyIsMissingAndAStandby_Then_CopiesTheStandby` - an empty body, or a missing body, gives way to the standby copy, which arrives one time;
  - `Test_runFolderSync_When_FirstStandbyCopiesTheMessage_Then_LaterSourcesAreNotFetched` and `Test_runFolderSync_When_FirstStandbyIsAlsoEmpty_Then_TheNextSourceIsTried` - the worker tries the standby sources in order and stops at the first usable body;
  - `Test_runFolderSync_When_EveryStandbyIsEmpty_Then_CountsOneErrorAndAppendsNothing` - when no standby has a usable body, the message counts one error and nothing reaches the destination;
  - `Test_runFolderSync_When_DestinationRefusesThePlannedMessage_Then_NoStandbyIsFetched` and `Test_runFolderSync_When_DestinationRefusesTheStandby_Then_CountsOneErrorAndTriesNoOtherSource` - a refusal by the destination is no reason to fetch a standby, and it ends the search;
  - `Test_runFolderSync_When_MainStreamFailsAfterTheRefusal_Then_TheStandbyStillCopies` - a stream error after the refusal does not stop the standby copy;
  - `Test_runFolderSync_When_BodylessResponseComesBeforeTheFullOne_Then_CopiesThePlannedMessageAndNoStandby` - when a response without a body comes before the full response, the worker copies the planned message and fetches no standby;
  - `Test_runFolderSync_When_ContextEndsDuringTheMainStream_Then_NoStandbyIsFetched` and `Test_runFolderSync_When_ContextEndsDuringTheStandbyStream_Then_ReturnsCanceledWithoutCounting` - a cancel stops the standby search and counts no error;
  - `Test_runFolderSync_When_StandbySourceUIDValidityChanged_Then_CountsOneErrorAndAppendsNothing` - a standby stream checks the UIDVALIDITY of its own source folder;
  - `Test_runFolderSync_When_NamesCarryControlCharacters_Then_TheStandbyLineShowsEscapes` - the verbose standby line escapes the names and the Message-Id;
  - `TestRunSync_When_TheFirstSharedMappingHasAnEmptyBody_Then_TheLaterMappingsCopyArrivesInTheSameRun` and `TestRunSync_When_SharedDestinationIsRecheckedAndTheReservedCopyHasAnEmptyBody_Then_TheStandbyArrives` - the standby copy arrives in the same run, also after the check of an existing destination;
  - `TestRunSync_When_EverySharedCopyHasAnEmptyBody_Then_EachRunFailsAndNothingEmptyIsAppended` - when every copy has an empty body, each run counts one error and the destination never gets an empty message;
  - `Test_maybeDiff_When_OneCopyPlanHasNoSharedDestination_Then_CutCopiesOfItsOwnFolderAreStandbys` - the copies that the rule cuts from the folder of the plan become standbys of the planned copy, in UID order. A single copy, or a Message-Id that the destination holds, gives none;
  - `Test_maybeDiff_When_ThreeOneCopyMappingsShareADestination_Then_OnlyTheFirstPlansAndTheOthersAreStandbys` - of three mappings with one destination, only the first mapping that lists the Message-Id plans a copy. The cut copies of that mapping come first as standbys. The copies of the other mappings follow, in mapping order. A destination that holds the Message-Id, also with a copy that has no UID, gives no plan and no standby;
  - `Test_maybeDiff_When_PlanCountsByCopiesAndHasNoSharedDestination_Then_PlanHasNoStandbys` - a plan that copies by count plans every copy and has no standbys;
  - `Test_maybeDiff_When_OneCopyMappingsShareASourceFolder_Then_TheFolderStandsByOnlyAsItsOwnPlan` - two mappings with one source folder and one destination add no second standby for that folder;
  - `TestFolderSyncPlan_mergeStandbys` - the merge sorts the standbys by planned UID, then by source, then by UID. The folder of the plan has the lowest source index, so it comes first for each planned UID. `standbysOf` gives the copies of one planned UID. The call never writes to the shared slices. A call that merges nothing, or takes the shared slices as they are, costs no allocation;
  - `Test_standbysFor_When_ManyCopiesOfOneSourceStandByForOnePlannedUID_Then_UIDsAscend` - copies of one source that arrive in random order come out in ascending UID order for each planned UID;
  - `Test_buildSyncPlan_When_OneCopyDestinationHasCutCopies_Then_PlansCarryOwnStandbysBeforeLaterMappings` - each plan lists the standbys of its own folder before those of later mappings. This holds for one mapping and for three mappings with one destination, also when a source scan or a destination scan is slow;
  - `Test_runFolderSync_When_PlannedCopyIsEmptyAndItsOwnFolderHoldsAnotherCopy_Then_CopiesItWithoutASecondExamine` - when the planned copy is empty, the worker first copies the standby from the same folder. This needs one EXAMINE of that folder. The worker goes to a later source only when that standby is also empty;
  - `Test_runFolderSync_When_AStandbyReplacesAPlannedUID_Then_TheVerboseLineGivesTheCause` - with `--verbose`, the line of a standby copy ends with the cause of the wait. Each of three cases gives one such line with one ending. A refused body gives `the copy in A has no usable body`, and the worker sends no UID SEARCH. A message that UID FETCH left out, and that UID SEARCH reports as held, gives `A did not deliver the planned copy`. A message that left the source after the plan gives `A no longer holds the planned copy`. In each case, the standby copy arrives one time and the run counts no error;
  - `Test_runFolderSync_When_OneStandbySourceHoldsSeveralCopies_Then_OnlyTheFirstUsableCopyIsAppended` - a standby source that holds several copies of the Message-Id gives one APPEND, with the first copy that has a body. When every copy is empty, the message counts one error;
  - `Test_runFolderSync_When_DestinationRefusesTheStandbyInVerboseMode_Then_LogsTheEscapedReason` - with `--verbose`, a destination that refuses the standby APPEND with a reason that holds a terminal control sequence gives one error. The log shows one "Failed to append" line for the refused body and one for the refused standby. It escapes the control character, and the worker tries no other source;
  - `Test_runFolderSync_When_ContextEndsDuringTheStandbyAppend_Then_ReturnsCanceledWithoutCounting` - a cancel that comes while the standby APPEND waits counts no error. The tracker shows `Canceled` and an error mark, and the worker tries no later source. The cancel comes after the server reads the literal;
  - `TestRunSync_When_LowestGmailCopyHasAnEmptyBody_Then_AHigherCopyOfTheMessageIdArrivesInTheSameRun` - when the lowest copy has no body, a higher copy of the same folder arrives in the same run. This holds also for a destination folder that LIST hides. The preview and the totals do not change, and a second run copies nothing;
  - `TestRunSync_When_FirstMappingOfAGmailDestinationHasAnEmptyBody_Then_AnotherCopyOfTheMessageIdArrivesInTheSameRun` - with a shared destination, another copy of the Message-Id arrives in the same run. The worker tries a copy of the same folder first, then a copy of a later mapping. This holds with one worker and with two workers, also for a destination folder that LIST hides;
  - `TestRunSync_When_EveryCopyOfAGmailMessageIdHasAnEmptyBody_Then_EachRunCountsOneErrorAndAppendsNothing` - when every copy of the Message-Id has an empty body, each run counts one error, ends with `ErrSilentExit`, and sends no APPEND;
  - `TestRunSync_When_SourceReturnsEmptyBody_Then_MessageFailsOnEachRun` and `TestRunSync_When_SourceBodyStaysEmpty_Then_EveryRunFailsLoudAndTheDestinationNeverGrows` - a message with an empty body fails on each run with exit code 1. The destination never gets an empty message.

  The UIDVALIDITY of a source folder has these tests:
  - `Test_buildSyncPlan_When_SourceFoldersHaveOwnUIDValidity_Then_EachPlanCarriesIt` - each plan holds the UIDVALIDITY of its own source folder;
  - `TestRunSync_When_SourceSendsNoUIDValidity_Then_SyncCopiesNormally` - a plan with no UIDVALIDITY is never checked, also in the check after CREATE;
  - `TestRunSync_When_SourceUIDValidityChangesBeforeCopy_Then_PlanFailsAndNothingIsAppended` and `TestRunSync_When_SourceStopsReportingUIDValidityBeforeCopy_Then_PlanFailsAndNothingIsAppended` - a changed value, or no value, ends the stream before any UID FETCH. The run exits with code 1, and the next run copies the rest;
  - `TestRunSync_When_SourceUIDValidityChangesBeforeRecheck_Then_FolderFailsBeforeCopy` and `TestRunSync_When_SourceStopsReportingUIDValidityBeforeRecheck_Then_FolderFailsBeforeCopy` - the check after CREATE gives a folder failure before any APPEND. This holds for a kept selection and for a new EXAMINE;
  - `TestRunSync_When_CreateReportsExistingEmptyFolderAndSourceIsRebuilt_Then_TheStreamStopsBeforeAnyAppend` - an existing empty destination needs no source scan, so only the stream catches the change. The stream stops before any APPEND, and the next run copies all messages;
  - `TestRunSync_When_OneOfTwoSourceFoldersIsRebuiltAfterThePlan_Then_OnlyThatFolderFails` - the other folder still copies, and the next run copies the failed folder;
  - `TestRunSync_When_RebuiltFolderNameCarriesControlCharacters_Then_FailureLinesShowEscapesAndWireNamesStayExact` - the failure lines escape the names, and the names on the wire stay exact;
  - `Test_runFolderSync_When_SourceUIDValidityChanged_Then_CountsAnErrorForEachPlannedMessageAndAppendsNothing` - the worker counts one error for each planned message, logs the stream error and appends nothing. The source gets no UID FETCH and no UID SEARCH;
  - `Test_runFolderSync_When_SourceUIDValidityChangedAndAStandbyExists_Then_TheStandbyOfAnotherFolderCopiesTheMessage` - a message with a standby in another folder still copies, and the check sends no UID SEARCH, because the UIDVALIDITY check fails first. If the folder of the plan is also the first standby source, its stream fails too, and the line ends with `(2 batches failed)`;
  - `Test_runFolderSync_When_StandbySourceHasItsOwnUIDValidity_Then_TheStandbyStreamUsesThatValue` - the standby stream checks the UIDVALIDITY of its own source folder, not the value of the plan. A changed value gives the stream line for that folder and one error.

  The accounting of planned messages has these tests:
  - `Test_runFolderSync_When_PlannedUIDsEndDifferently_Then_EachIsSyncedFailedOrGoneOnce` - in each case, synced, errors and gone add up to the planned messages. The cases are:
    - every message delivered, and a response without a body before the full one;
    - a held message that UID FETCH leaves out, and a response of a held message with no `BODY[]`;
    - a message that left the source;
    - a held message or a gone message with a usable standby or an empty standby;
    - a refused check, and a changed UIDVALIDITY;
    - a tagged NO after every body, and a second batch that fails before it delivers anything;
    - one message of each kind;
  - `Test_runFolderSync_When_ABatchHoldsAnUnreadableMessage_Then_LaterBatchesAreCopied` - one unreadable message in a batch costs one error. The later batches copy, the check searches only for that UID, and the log holds one stream line;
  - `Test_runFolderSync_When_SeveralBatchesFail_Then_OneLineCountsThem` - three failed batches give one stream line with `(3 batches failed)`, and only the two held messages count as errors;
  - `Test_runFolderSync_When_SourceFolderCannotBeOpened_Then_EveryPlannedMessageIsAnError` and `Test_runFolderSync_When_SourceFolderCannotBeOpenedAndAStandbyExists_Then_TheStandbyOfAnotherFolderCopiesTheMessage` - when the source refuses EXAMINE, the worker sends two EXAMINE commands (the stream and the check), no UID FETCH and no UID SEARCH. All 1001 planned messages count as errors, and the run records no stop. With a standby in another folder, the standby copies the message, and the tracker ends as done;
  - `Test_runFolderSync_When_SourceThrottlesTheFirstBatch_Then_EveryPlannedMessageIsAnErrorWithoutACheck` - a throttled first batch ends the stream with one UID FETCH. All planned messages count as errors, and the source gets no UID SEARCH;
  - `Test_runFolderSync_When_MainAndStandbyStreamsFail_Then_OneLineNamesTheFirst` - the failed batches of the main stream and of a standby stream add up in one line, which names the folder of the first failure. The message still copies from the standby;
  - `Test_runFolderSync_When_ContextEndsAfterARefusedBatch_Then_PrintsNoStreamLine` and `Test_runFolderSync_When_ContextEndsDuringTheCheck_Then_ReturnsCanceledWithoutGoneLine` - a context that ends gives no stream line and no gone line. The tracker shows `Canceled` and an error mark;
  - `Test_runFolderSync_When_GoneNamesCarryControlCharacters_Then_TheGoneLineShowsEscapes` - the gone line shows control characters in the folder names as escapes;
  - `Test_runFolderSync_When_EventsAreRandom_Then_EveryPlannedMessageEndsOnceAndNothingIsSentTwice` and `TestRunSync_When_SourceEventsAreRandom_Then_NoMessageIsStoredTwiceAcrossTwoRuns` - for random events, each planned message ends once. The events include a response without a body before the full one, an empty body and no body item. They include a message that FETCH leaves out and a message that left the source. They include a standby, a refused APPEND, a refused batch, a changed UIDVALIDITY, a refused check and one throttled reply. The destination stores no message twice, also across two runs;
  - `TestRunSync_When_SourceSendsABodylessResponseBeforeEachFullOne_Then_RunSucceedsWithoutErrors` - a response without a body before each full response gives no error;
  - `TestRunSync_When_SourceLeavesAHeldMessageOutOfUIDFetch_Then_EveryRunFailsAndTheDestinationNeverGetsIt` and `TestRunSync_When_SourceAnswersWithoutUID_Then_TheMessageIsAnErrorOnEveryRun` - a message that the source holds but does not deliver, or answers without a UID, counts as one error on each run. The check sends one UID SEARCH for that UID, the destination never gets the message, and the output has no gone line;
  - `TestRunSync_When_PlannedMessageLeavesTheSourceBeforeTheCopy_Then_RunSucceedsAndNamesTheGoneMessage` - a message that leaves the source after the plan counts as gone. The run succeeds, the output shows the gone line one time (not with `--quiet`), and a second run finds nothing to copy;
  - `TestRunSync_When_ExistenceCheckAnswersOrIsRefused_Then_OnlyAnAnsweredCheckCountsAMessageAsGone` - with a working check, one held message is an error and one gone message is gone. With a refused check, both count as errors, and the output has no gone line;
  - `TestRunSync_When_OneMessageCannotBeFetched_Then_TheRestOfTheFolderIsCopied` and `TestRunSync_When_AWholeBatchIsRefusedInQuietMode_Then_OtherBatchesCopyAndTheSummaryCountsEveryUndeliveredMessage` - a refused batch costs only its messages. The other batches and the other folders copy. The run prints one stream line for the folder. The summary counts each undelivered message as an error. A second run tries the undelivered messages again;
  - `TestRunSync_When_SourceFolderIsRefusedAfterThePlan_Then_OneExamineEndsItsStreamAndOtherFoldersCopy` - a folder that the source refuses to open after the plan gets three EXAMINE commands (the plan scan, the stream and the check). All its messages count as errors, and the other folders copy.

  The flags and the date of a copied message have these tests in `internal/app`:
  - `TestRunSync_When_SourceMessagesHaveFlags_Then_EachCopyKeepsTheFlagsOfItsSource` - each copy gets the flags of its source message, without `\Recent`, `\Deleted` and the other system flags. A message with no flag to carry gets no flag list, and the spelling of the flags stays;
  - `TestRunSync_When_SourceFlagsChangeAfterTheCopy_Then_ALaterRunLeavesTheDestinationFlags` - a later run sends no APPEND and no STORE, so the destination keeps the flags of the first run;
  - `TestRunSync_When_SourceFlagListHoldsCommandText_Then_DestinationGetsOnlyValidFlagsAndNoOtherCommand` - a flag list with a literal and a DELETE command gives one APPEND with the valid flag only. The destination sees no DELETE, and its folder `Work` stays;
  - `TestRunSync_When_DestinationRefusesFlagExtensions_Then_TheMessageIsCopiedOnceWithoutThem` - a destination that refuses a flag extension gets the message one time without it, also when the extension names a quota;
  - `TestRunSync_When_SourceMessagesHaveInternalDates_Then_EachCopyKeepsTheDateOfItsSource` - each copy gets the INTERNALDATE of its source. When the stream cannot read the date, or the source sends no date, the APPEND uses the `Date` header. With neither, the APPEND has no date;
  - `Test_runFolderSync_When_StandbyCopyHasOtherFlags_Then_TheAppendCarriesTheStandbyFlags` and `Test_runFolderSync_When_StandbyCopyHasOtherInternalDate_Then_TheAppendCarriesTheStandbyDate` - a standby copy brings its own flags and its own date;
  - `TestRunSync_When_LowestGmailCopyHasAnEmptyBody_Then_TheHigherCopyKeepsItsOwnFlagsAndDate` - when the lowest copy has an empty body, the higher copy of the same Message-Id keeps its own flags and date.

  A refused CREATE has these tests:
  - `Test_splitRefusedDestinations` - the plans of a refused destination become folder failures that wrap `errDestinationCreate` and the error of the server. They keep the plan order, and the other plans stay in order. A shared destination fails every plan of it, and an empty or nil map keeps every plan;
  - `TestRunSync_When_OneCreateIsRefused_Then_OtherFoldersStillCopy` - the other folders copy, the server does not create the refused folder, and it gets no APPEND, and the failure list shows before the copy. The run ends with `ErrSilentExit` and counts one failed folder. A second run sends the CREATE again and opens no worker connection, because only the refused plan remains;
  - `TestRunSync_When_EveryCreateIsRefused_Then_FolderFailsAndNoWorkerConnects` - when no plan remains, the run opens no worker connection and ends with exit code 1. With `--quiet`, the failure list shows and the `Failed to create folder` log does not;
  - `TestRunSync_When_CreateOfASharedDestinationIsRefused_Then_EveryMappingOfItFails` - both mappings of a refused shared destination fail, and the destination gets no EXAMINE. The pool holds one worker for the one plan that remains;
  - `TestRunSync_When_ParentCreateIsRefused_Then_FoldersBelowItFail` - a refused parent fails each folder below it with the error `failed to create parent folder NAME`. This holds when the parent has a plan and when it has none;
  - `TestRunSync_When_ContextEndsDuringCreateAfterARefusal_Then_ReturnsCanceledWithoutFailureList` - a context that ends after a refusal returns `context.Canceled` and prints no failure list;
  - `TestRunSync_When_CreateIsRefusedAndAnotherDestinationIsHidden_Then_OnlyTheHiddenOneIsRechecked` - the refused folder gets no check after CREATE, and the hidden folder gets its check;
  - `TestRunSync_When_FolderFailsAndCreateIsRefused_Then_BothFailuresAreListedInOrder` - the failure of the scan shows first, the failure of CREATE shows after the creation, and the final line counts both folders;
  - `TestRunSync_When_CreateRefusalCarriesControlCharactersInQuietMode_Then_TheFailureListShowsEscapes` - with `--quiet`, the failure list shows the folder name and the reply with escapes;
  - `TestRunSync_When_FirstPlanOfAHiddenSharedDestinationIsRebuiltBeforeTheRecheck_Then_OnlyItFailsAndTheNextRunCompletesTheFolder` - the first plan of a hidden shared destination fails at the check because its source changed the UIDVALIDITY. The second plan copies, and the next run completes the folder.

  The mapping precedence and the source conflicts have these tests:
  - `Test_mostSpecific` - the longest root wins in any order. Equal roots keep the first entry. Two spellings of one INBOX level give the smaller root in byte order;
  - `Test_checkSourceConflicts` - equal items and distinct sources pass, and two destinations for one source give an error that names both items;
  - `Test_expandMappingsWithSubfolders_When_ExplicitMappingsNest_Then_MostSpecificWinsInEveryOrder` and `TestRunSync_When_ThreeNestedMappingsAreGiven_Then_MostSpecificWinsInEveryOrder` - the nearest mapping decides, whatever the order of the items;
  - `Test_expandMappingsWithSubfolders_When_ChildMappingOverridesParent_Then_VerboseNamesBothDestinations` - with `--verbose`, the line "goes to ..., not to ..." names both destinations;
  - `Test_expandMappingsWithSubfolders_When_ExplicitChildIsNoSelectOrUnlisted_Then_ItsChildrenFollowIt` - the children of a mapped `\Noselect` or unlisted folder follow the mapping of that folder;
  - `Test_expandMappingsWithSubfolders_When_ExplicitDestinationEqualsRenamedDerivedName_Then_ExplicitEntryIsKept` - an explicit mapping keeps its typed destination when it equals a renamed derived name, and its entry is not marked renamed;
  - `Test_expandMappingsWithSubfolders_When_OverrideNamesCarryControlCharacters_Then_LineShowsEscapes` - the override line escapes the names;
  - `Test_expandMappingsWithSubfolders_When_QuietAndVerboseAreBothSet_Then_NothingIsPrinted` - `--quiet` hides the override lines;
  - `TestRunSync_When_ParentAndChildFoldersAreBothMapped_Then_ChildMappingWinsInEitherOrder` - the child mapping decides, and a second run sends no APPEND;
  - `TestRunSync_When_TwoMappingsNameOneSourceWithDifferentDestinations_Then_RunStopsBeforeThePlan` - two spellings of INBOX, or a rewritten delimiter, make one source and stop the run before the scan;
  - `TestRunSync_When_MapRepeatsAnItemExactly_Then_RunMatchesASingleItem` - a repeated item causes no extra scan, CREATE or APPEND;
  - `TestRunSync_When_MirrorHoldsNestedAndRenamedFolders_Then_NoMappingOverridesAnother` - in a mirror run, the own mapping of each folder wins;
  - `TestRunSync_When_ExplicitChildDestinationEqualsRenamedSiblingDestination_Then_BothFailAsAClash` - a typed destination that equals a renamed destination gives a clash, and both folders fail.

  The throttle stop and the destination that gave up have these tests:
  - `TestRunSync_When_DestinationThrottlesAppend_Then_RunStopsWithoutDownloadingTheRest` - for the Gmail reply and the Dovecot reply, with one worker, one APPEND and one UID FETCH of `1:500` go to the servers. The run prints the stop line one time, counts all 603 messages as errors, and ends with `ErrSilentExit` and no `Cancelled`. A `--quiet` case shows the stop line and the summary;
  - `TestRunSync_When_DestinationGivesUpMidPlan_Then_NoMoreBodiesAreFetched` - a destination that drops the connection at its second APPEND and refuses every login gives one UID FETCH of `1:500`. The second plan sends no source command, and the run prints no stop line;
  - `TestRunSync_When_SourceThrottlesTheBodyFetch_Then_RunStops` - a throttled UID FETCH names the source in the stop line. The second plan does not start, and the counts cover both plans;
  - `TestRunSync_When_TwoWorkersAndTheDestinationThrottles_Then_NoPlanStartsAfterTheStop` - in every interleaving, at most two APPEND commands and two UID FETCH commands of `1:500` go to the servers. The third plan does not start;
  - `TestRunSync_When_ThrottleStopsTheRun_Then_TheNextRunCopiesTheRest` - the first run copies 3 messages and stops. The second run copies the rest and succeeds;
  - `TestRunSync_When_StopReasonCarriesControlCharacters_Then_TheStopLineShowsEscapes` - the stop line shows the control and bidi characters of the reply as escapes;
  - `Test_runFolderSync_When_RunIsAlreadyStopped_Then_SendsNoCommand` - a stopped run sends no command on either worker Client. Each message counts as an error, and the tracker shows `Stopped`;
  - `Test_runFolderSync_When_AnotherWorkerStopsTheRunDuringAnAppend_Then_NoFurtherAppend` and `Test_runFolderSync_When_TheRunStopsAtTheLastMessageOfABatch_Then_NoNextBatch` - a stop from another worker ends the plan before the next APPEND and before the next batch;
  - `Test_runFolderSync_When_AppendFailsAfterAnotherWorkerStoppedTheRun_Then_ThePlanEndsAtOnce` - a refused APPEND on a working session ends the plan when a stop exists, and each message counts as an error;
  - `Test_runFolderSync_When_DestinationThrottles_Then_RecordsTheDestinationAndStops`, `Test_runFolderSync_When_ExistenceCheckIsThrottled_Then_RecordsTheSourceAndSkipsStandbys` and `Test_runFolderSync_When_StandbySourceThrottles_Then_RecordsTheSourceAndTriesNoOtherSource` - the stop holds the side and the reply, and the worker tries no standby source;
  - `Test_runFolderSync_When_AThrottledStopFollowsARefusedBody_Then_NoStandbyIsFetchedAndBothAreErrors` - a throttled source stop after a refused body fetches no standby, and the waiting UID counts as an error;
  - `Test_runFolderSync_When_StandbyAppendFindsTheDestinationGone_Then_NoOtherSourceIsTried` and `Test_runFolderSync_When_TheRunStopsDuringAStandbyStream_Then_NoLaterSourceIsTried` - a standby search ends when the destination gave up or the run stopped, whether the standby stream delivers a message or not;
  - `Test_runFolderSync_When_DestinationGaveUpBeforeThePlan_Then_NoSourceCommand` - a destination that gave up gets no dial and no command, the source gets no command, and each message counts as an error;
  - `Test_runFolderSync_When_AppendIsRefusedOnALiveSession_Then_ThePlanGoesOn` - a refusal that is no throttle counts one error for each message and records no stop;
  - `Test_streamFailure` and `Test_streamFailure_When_StopIsThrottledAndFirstIsANetworkError_Then_ReportsTheThrottle` - `streamFailure` reads the throttled `Stop` of a stream error, also when `First` is a network error;
  - `Test_throttleStop_When_SeveralRepliesAreRecorded_Then_TheFirstOneWins` and `Test_throttleStop_When_WorkersRecordAtTheSameTime_Then_OneCauseStaysForEveryReader` - the first reply wins. `recordThrottled` reports true only for a throttled reply. When 16 workers record at the same time, they leave one cause that each of them reads;
  - `Test_runFolderSync_When_ExamineGetsAThrottledReply_Then_TheStopNamesTheSourceAndNoCommandFollows` - a throttled EXAMINE records the source, sends no UID FETCH and no UID SEARCH, and counts all planned messages as errors;
  - `Test_runFolderSync_When_ARefusedBatchIsFollowedByAThrottledBatch_Then_OneLineCountsBothAndTheStopNamesTheSource` - the line counts both batches and ends with `(2 batches failed)`, and the stop names the source. The bodies that arrived before the status copy;
  - `Test_runFolderSync_When_TheRunStopsAtTheLastMessageOfAStandbyBatch_Then_NoNextStandbyBatch` - a stop that falls on the last message of a standby batch sends no next standby batch;
  - `TestRunSync_When_SourceThrottlesAfterTheLastBodyIsCopied_Then_RunStopsAndTheNextRunHasNothingToCopy` - a throttled status after the last body still records the stop. The run prints the stop line and ends with `ErrSilentExit`, also with `--quiet`, with 0 errors. The check sends no UID SEARCH, and the next run finds nothing to copy;
  - `TestRunSync_When_DestinationThrottlesWithBodiesInFlight_Then_TheClosedSourceSessionCostsNoLogoutWait` - the source session that the stop closed fails its LOGOUT at once, so the run does not wait for the logout limit;
  - `TestRunSync_When_DestinationThrottlesBeforeTheNextPlan_Then_ThePlansThatNeverStartedShowStopped` - plans that never started show `Stopped` in the tracker. They count all their messages as errors. They send no command to the source after the planning scan;
  - `TestRunSync_When_CreateGetsAThrottledReply_Then_RunStopsBeforeAnyWorker` - a throttled CREATE reply sends no more CREATE, no APPEND and no worker LOGIN. The destination gets no EXAMINE, STATUS or FETCH. The run prints the stop line one time and counts all planned messages as errors. The `Failed to create folder` log shows only without `--quiet`. After the reply stops, the next run succeeds;
  - `TestRunSync_When_CreateIsRefusedAndThenThrottled_Then_OnlyTheRefusedFolderFails` - a folder that CREATE refused before the throttled reply keeps its failure. The throttled folder and the folders after it have none. The failure list shows once with `--quiet` and twice without it, always before the stop line;
  - `TestRunSync_When_DestinationRecheckIsThrottled_Then_RunStopsBeforeAnyWorker` and `TestRunSync_When_SourceRecheckIsThrottled_Then_RunStopsBeforeAnyWorker` - in the check after CREATE, a throttled reply to the read of the destination stops the run. A throttled reply to the scan of a source stops the run also. The run stops before any worker starts and before any APPEND. Each planned message counts as an error. The stop line shows one time;
  - `Test_recheckExistingDestinations_When_DestinationScanIsThrottled_Then_ReturnsThePlansAndSendsNothingMore` and `Test_recheckExistingDestinations_When_SharedSourceScanIsThrottled_Then_NoLaterSourceIsScanned` - the call records the stop and returns the plans as they were, with no failure and no line. No later group and no later source gets a command;
  - `TestRunSync_When_ContextEndsWithAThrottledReply_Then_ReturnsCanceledWithoutStopLine` - a context that ends with a throttled reply, in CREATE or in the check after CREATE, gives `context.Canceled` and no stop line.

  Each stop test checks that synced, errors and gone add up to the planned messages.

  The case-variant warning has these tests:
  - `Test_printCaseVariantDestinations` - the groups, their order, the escapes and the rule that a group with every name listed gets no line. Four cases repeat a spelling. In two cases, the line names the repeated spelling one time. In one case, two mappings name one destination with no other spelling, so no line shows. In one case, the LIST holds every spelling, so the group gets no line;
  - `Test_printCaseVariantDestinations_When_GroupHoldsEveryLetterCaseVariant_Then_ListsEachNameOnce` - the test gives the warning all 16384 letter-case spellings of one name, in reverse order, with three of them repeated. When the LIST misses one spelling, the line names each spelling one time, in byte order. When the LIST holds every spelling, the line does not show. The call changes neither the mappings nor the LIST;
  - `TestRunSync_When_DestinationsDifferOnlyInLetterCase_Then_WarningShowsAndEachFolderCopies` - the warning shows before the prompt, also with `--quiet`, and each folder gets its messages.
- **`internal/progress`** - interface compliance against the contract `internal/client` expects.
- **`internal/termsafe`** - `Escape` and `Writer` have 100% coverage. The tests are table tests, a fuzz target, `Writer` tests and a test that `Escape` does not allocate memory when nothing needs escaping.
- **Terminal output in `internal/client` and `internal/app`** - end-to-end tests with fake servers use a folder name, a reply and a Message-Id that contain control and bidi characters. They check the escaped output, and they check that the names on the wire stay byte-exact. These tests include the list of folders that cannot be synced, the skip line, and the list of folders that `show` cannot read. They also include the left-out lines, the rename notice and the "already exists and holds" lines (`TestRunSync_When_ExistingDestinationNamesCarryControlCharacters_Then_InfoLineShowsEscapesAndServersGetExactNames`). They include the case-variant warning, the mapping override line and the standby line. They also include the list of folders to create for a shared destination and the failure lines of a rebuilt source folder.
- **`cmd/imapsync-go`** - `TestReportError` covers the exit code and the escaped `Error:` line. `TestFetchTag` covers `helpers.fetchTag` against an `httptest` server.

## What we deliberately don't test

If a behaviour is hard to test, test it at integration level. If you cannot do that, record it in this file as a deliberate exclusion and give the reason. These are examples of acceptable exclusions:

- The TLS handshake itself (delegated to stdlib).
- `goreleaser` output (delegated to goreleaser).
- The `urfave/cli/v3` flag parsing wiring in `cmd/imapsync-go/main.go` (declarative; failure mode is a CLI parse error from the library).

### Deliberate exclusions in `internal/client`

- **`client.New`** - the `Test_New_*` tests cover CRAM-MD5, dial failure, LIST refusal, CAPABILITY refusal, cancel during LOGIN, cancel during the CAPABILITY read and shared limiters. Only `internal/app` tests cover two branches: the `tls.Dialer` path and the default dial timeout. The `tls.Dialer` test uses a plaintext server, so a handshake error is expected. The standard library handles a successful TLS handshake, so no test covers it.
- **Goroutine count after a failed `client.New`** - a goroutine count is not stable in a process that runs parallel tests. So the tests for a connection that drops during the capability check do not count goroutines. They prove that the client closes every socket:
  - the fake server keeps each session open until the client hangs up. `dropThenHold` half-closes the first session like a dropped connection. `holdAfterLoginReply` answers LOGOUT without a BYE, so only the client can close the session;
  - each test waits until the server sees the end of every session, so it fails when `New` leaves one session open;
  - `disableGC` stops a finalizer from closing a socket that the code leaked. This setting is process-wide, so these two tests run in sequence;
  - `Test_New_When_ConnectionDropsDuringCapabilityCheckAndReloginIsRefusedPermanently_Then_FailsAndClosesEverySocket` also checks the error text, the class `ClassPermanent` and two LOGIN commands;
  - `Test_New_When_ConnectionDropsDuringCapabilityCheckAndEveryReloginFailsTemporarily_Then_FailsWithTheLabel` also checks that the error names the label. It checks one LOGIN for the first session and one for each reconnect attempt.
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
- **`StreamMessagesByUIDs`, the status check after the kept-selection fallback** - `status` is nil there only when a BYE arrives between the mailbox read in `selectIfNeeded` and the fallback read. go-imap v1 sets `LogoutState` and clears the mailbox under one lock, so the UID FETCH returns `ErrNoMailboxSelected` and sends no command. `classifyError` treats this error as `ClassTransient`, so `safeCall` reconnects and the retry session runs the check again. A deterministic test needs a hook between the two mailbox reads. `Test_StreamMessagesByUIDs_When_KeptSelectionHasOtherUIDValidity_Then_FetchesNothing` covers the kept-selection path on both sides of the UIDVALIDITY comparison.
- **The buffer of the scans that fetch no body** - `messageChanBuffer` only sets how far the reader goroutine runs ahead of the scan loop. Two scans use it: the Message-Id scan in `fetch.go` (`scanMessageIDs`) and the size scan of `ListMailboxes` in `mailbox.go` (`getFolderSize`). Neither loop waits for a caller, so no command, count or result depends on the size. `Test_StreamMessagesByUIDs_When_CallbackBlocks_Then_AtMostOneBodyIsReadAhead` tests the body stream, which must not use the buffer.

### Deliberate exclusions in `internal/app`

- **`runFolderSync` post-append `ctx.Err()` check** - The check stops the loop when an APPEND finishes after the context is canceled. `withCancel` registers a `context.AfterFunc` that calls `Cancel()`, and `Cancel()` terminates the connection asynchronously. An APPEND that finishes at the same time can return nil, not an error. This is a race window, and the test has no deterministic hook to reach it. The `(synced, errors)` result is the same with or without the check. The check saves at most one extra iteration.
- **`app.traceTracker`** - a diagnostic that writes to stderr. It is active only when `IMAPSYNC_TRACE` is set.
- **`ActionSync` verbose `Fetching config...` line** - `ActionSync` writes it to `os.Stdout` before the config loads. It is the only output that does not go through the injected writer.
- **Boundary `ctx.Err()` re-checks** - each check reads the context of the caller between two calls that give no observable event. A test can reach such a check only with a context that ends on a given call of `Err`. That test depends on the number of `Err` calls in `internal/client` and in the code before the check, so a refactor that keeps the behaviour breaks it. `TestRunSync_When_CanceledAtPhaseBoundary_Then_NothingMoreIsSent` and `TestRunSync_When_CanceledDuringAppend_Then_NoFurtherPlanIsStarted` test the end-to-end property. The re-checks are in these places:
  - `runSync`, after the connect errgroup;
  - `runSync`, at the top of the dispatch loop;
  - `expandMappingsWithSubfolders`, in the per-mapping loop, after `getSubfolders` and in the per-subfolder loop;
  - `isHierarchyLevel`, the error return after `getSubfolders`;
  - `runSync`, the stop checks of the dispatch loop, at its top and after the wait for a worker. A stop that lands between two iterations has no hook. `runFolderSync` repeats the check before its first command, so a run without these checks gives the same counts, output and commands. `TestRunSync_When_TwoWorkersAndTheDestinationThrottles_Then_NoPlanStartsAfterTheStop` tests the property in each interleaving;
  - `runFolderSync`, the context check after a failed `EnsureSession`, before the plan and after a failed APPEND. `Test_EnsureSession_When_ContextEndsDuringReconnect_Then_ReturnsContextError` covers the call.
- **`buildSyncPlan`, the `MailboxExists` error in the destination goroutine** - `MailboxExists` reads the mailbox cache, so it fails only when the context ends. The `gCtx.Err()` check at the top of the loop runs first. `gCtx` comes from `errgroup.WithContext`, so a test cannot replace its `Err`. So the error needs a cancel in the short time between the two calls. A deterministic test needs a production hook there. `Test_buildSyncPlan_When_ContextEndsAfterEntryCheck_Then_NeitherScanStarts` covers the check at the top of the loop.
- **The throttled give-up of `EnsureSession` before a plan** - a throttled give-up comes from `reconnect`, and `reconnect` waits `throttledBackoff = 5m` between login attempts. An end-to-end test needs a wait of 5 minutes. `Test_EnsureSession_When_GiveUpIsThrottled_Then_IsThrottledReportsIt` and the gate tests cover the client side. `Test_runFolderSync_When_DestinationThrottles_Then_RecordsTheDestinationAndStops` and the throttled APPEND tests cover `recordThrottled`, which the give-up path shares.
- **The revive login before a plan after `reviveInterval`** - an end-to-end test needs a wait of `reviveInterval = 5m`. `Test_EnsureSession_When_ReviveIntervalPassed_Then_OneLoginDecides` covers the login attempt with an expired cool-down.
- **A refusal before the `+` continuation, end to end** - go-imap drops the `false` continuation signal when the writer does not yet wait, so the test can hang. `TestIsThrottled` covers the classification of the text that go-imap returns.
- **A Ctrl-C after a throttle stop, end to end** - the context must end after a worker records the stop and before `runSync` reads the context. No hook sits between these two points, and a cancel inside an APPEND hook can hang (see the next item). `Test_runFolderSync_When_RunIsStoppedAndContextEnds_Then_ReturnsCanceledWithoutCommands` tests that a cancel wins over a stop in one plan, and `TestRunSync_When_CanceledDuringAppend_Then_NoFurtherPlanIsStarted` tests the check of `runSync`.
- **A cancel before the `+` continuation of an APPEND** - the cancel closes the socket, and go-imap then waits on the `continues` channel with no end (see the Known limitation in `CLAUDE.md`). So a test of this window can hang. A `fakeServer.onCommand` hook for APPEND runs before the fake server sends `+`. Do not cancel in such a hook. `TestRunSync_When_CanceledDuringAppend_Then_NoFurtherPlanIsStarted` cancels through `scriptedHandler`, which reacts after the server reads the literal.

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
