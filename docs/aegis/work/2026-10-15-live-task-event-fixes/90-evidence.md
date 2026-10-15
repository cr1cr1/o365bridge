# Evidence

## Local delivery gates

Description repair `5cdf4e4` passed full race tests, five affected repetitions, vet/build and independent review before commit. Its red test reproduced the exact ALTREP/data-URI prefix observed in Outlook notes. Owner-run task update/create at11:22 subsequently showed clean plain native Body and matching native readback.

The recurring-task correction passed parent final job bash-84, collected exit0:

```sh
export GOMODCACHE="$PWD/.gocache/mod" GOCACHE="$PWD/.gocache/build" GOPATH="$PWD/.gocache/gopath" GOSUMDB=off
go test -race ./... -count=1
go test -race ./internal/davserver ./internal/owa -count=5
go vet ./...
go build -o dist/o365bridge .
git diff --check
```

All repository packages passed, including SMTP. Independent reviewer348ee7c3 reviewed the final post-write error guard and connected OWA/registry/DAV flow: Ship, no remaining concrete must-/should-fix code findings. Review did not substitute for parent execution.

## Acceptance coverage

- Native fields exactly identify active master versus detached incomplete task, even with copied Recurrence and UID metadata.
- TaskDeadOccurrence namespace/type/value/presence checks, including equivalent GUID and contradictory/unknown inputs.
- All-page hydration before UID projection; read/order/page/provider reconstruction stability; no row loss or read-time cloud metadata mutation.
- Sole final and ordinary task UID preservation; fail-closed unknown/multiple ownership and derived UID collisions.
- Changed response native ID causes one own-property-only UID assignment to that ID; same/missing response ID does not. Initial/repair errors propagate.
- Native split followed by completion/deletion/reconstruction preserves write-assigned detached UID.
- Native HTTP + real DAV fixture: date edit, original continuing href, distinct one-off href without active repetition, complete query and selective multiget, explicit test-series deletion without detached rename.
- Recurring task create/edit/clear PUT validator omission; ordinary validators and fresh-GET conditional edits preserved.
- Real HTTP401 + Registry.Call regression: before fix post-write auth recovery replays three actual native edits; after fix exactly one successful edit and one repair attempt, no recovery/replay. Initial-auth positive control still renews safely.

## Live evidence and remaining checks

Original runtime capture: task UpdateItem134 generated a new native ID while advancing the original; multiget returned two rows for one UID/href. Fresh owner restart/sync at11:39 confirmed native TaskDeadOccurrence strings true on detached rows and false plus IsRecurring=true on masters, across two copied-UID groups. Both incomplete; completion/date/title ordering was not used to classify ownership.

Owner restarted/synchronized the final build at12:25:40; native listing read all four tasks successfully. Parent read-only live CalDAV receipt (job bash-130, collected exit0) then verified HTTP207 full query: four task resources, four distinct hrefs, four distinct UIDs, two active series and two nonrecurring instances, four canonical validators. Each of four single-href multigets returned exactly one matching response. The probe used the existing local bridge configuration without printing credentials/personal content, proxies or redirects, and performed no PUT/DELETE/cloud metadata write.

This proves canonical legacy task resources are independently addressable at the live protocol boundary without dropping rows, NOT that Thunderbird's full symptom is resolved. The client separately requested two unknown hrefs and received zero matches. User follow-up reports old cloud tasks still absent locally, while a new standalone task and its plain-note edit succeed and stay visible. Native create10 at12:41:11 and note update23 at12:41:22 succeed with selective readback1. Existing client requests still show requested_hrefs2/returned0.

Converting that new task to recurring then fails: native UpdateItem42 at12:41:37 returns ErrorInvalidPropertyRequest; MessageXml identifies calendar:Recurrence being applied to Task. This is a separate task update-field URI defect, not evidence to discard the verified identity/retry fixes. Next slices must diagnose unmatched hrefs and correct task recurrence SET/DELETE paths with strict native regressions. Future split UID assignment/auth-failure handling remain regression-tested rather than claimed final live writes; no appointment-event write has appeared. Raw logs/credentials/cloud identifiers are not copied into this record.

## Task URI and Thunderbird path follow-up

Task-specific recurrence SET/DELETE paths are committed as `218ba6e`; strict native HTTP tests reproduce the captured `calendar:Recurrence` rejection. Fresh independent reviewer129b7973: Ship. The separate CalDAV href slice has source-backed Mozilla normalization tests, object alias CRUD/account+collection guards, explicit missing404 and ambiguity409 tests; independent reviewer780ee5c7: Ship.

Parent combined final job bash-303 collected exit0 for full race suite, five repeated DAV/OWA race suites, vet, dist build and diffcheck. Parent restarted only the existing process-compose `run` through controller18099 (no configuration changes/replacement server). Read-only receipt bash-311 collected exit0: all5currentresources have distinct hrefs/UIDs, all5canonical and all5Mozilla-normalized single-resource multigets return exactly1matchingUID. Before correction, receipt195 measured5canonical versus3Mozilla successes.

Live user request220 at13:42:51 then successfully SET TaskRecurrence (`NoError`, DAV204); actual client multiget at13:42:54 requested2/returned2/missing0/duplicate0. Request261 generated a legitimate new native split ID and UID-only follow-up262 succeeded, confirming the earlier metadata assignment against this tenant. Subsequent new standalone create293, ordinary edit308 and recurring SET323 succeeded and stayed visible according to user.

User reports a later repeat-removal did not reach cloud. Through the current captured tail, no TaskRecurrence DELETE or native write follows SET323; current native task remains recurring. This does not prove native deletion failed: a downstream precondition/parser rejection or missing client PUT must first be distinguished. Failed DAV mutation statuses currently are not logged, so this remains active diagnosis rather than claimed completion. Reminder editing, exact percentage and attachment upload remain incomplete; no promise of those fields is made.

## Reminder protocol follow-up

Later owner retry at14:43:37 sent TaskRecurrence DELETE84, got native NoError/DAV204, and readback omitted recurrence. The earlier no-provider-write observation was not falsely classified as a native DELETE failure.

The latest user explicitly identified reminder edits as the remaining gap. Task VALARM was ignored and the handler forced preservation of the old value. The new scoped codec maps one DISPLAY alarm to the already-supported reminder instant, and a transient ReminderUnchanged flag prevents invisible precision loss on unrelated edits. Worker scoped DAV/OWA/Graph suites passed; parent native HTTP/DAV test351 passed create/read, 100ns precision with zero note-edit reminder writes, relative END change, explicit clear, and no extra recurrence/creation.

Parent FINAL reminder job357 collected exit0:

```sh
go test -race ./... -count=1
go test -race ./internal/davserver ./internal/owa ./internal/graph -count=5
go vet ./...
go build -o dist/o365bridge .
git diff --check
```

Independent final reviewer952fc578: Ship, no concrete must-/should-fix. Alarm input/cardinality/nesting, timezone/DST/overflow, provider create/update/partial isolation, exact pointer/skip-write preservation and canonical ETags were inspected.

Parent restarted only the existing process-compose run (receipt360 exit0; expected unauthenticated DAV401 listener health, no task request). Owner then selected “Reminder updates correctly.” In the final15:25 runtime window, native UpdateItem21 set ReminderIsSet=true plus ReminderDueBy and returned NoError; GetItem25 returned the same enabled reminder instant. UpdateItem88 changed ReminderDueBy and returned NoError; GetItem91 returned the changed enabled instant. Intervening recurring-task edits34/69 emitted no reminder fields and their readbacks retained the reminder, confirming unrelated-edit preservation against this tenant. The actual task identities/titles and timestamps are not copied here. A live reminder-clear request was not present in this final window; clear is covered by the native HTTP/DAV lifecycle regression, not falsely claimed as live-tested. No appointment-event write was captured.

## Explicit residual boundaries

Native date edits/completion can intentionally retain a continuing series plus detached cloud occurrence; no row deletion/automatic merge is implemented. New split UID assignment is a non-atomic second write; errors surface without internally replaying the committed first edit. Older copied-UID groups retain bounded read compatibility only: after generator removal a sole survivor can fall back to its stored series UID, or multiple survivors can be ambiguous. No bulk migration is performed. Mixed DATE/date-time and equal native start/due values remain a separate strict RFC5545 fidelity gap; no timestamp/precision guessing is included.
