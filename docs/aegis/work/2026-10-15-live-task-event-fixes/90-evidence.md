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

## Explicit residual boundaries

Native date edits/completion can intentionally retain a continuing series plus detached cloud occurrence; no row deletion/automatic merge is implemented. New split UID assignment is a non-atomic second write; errors surface without internally replaying the committed first edit. Older copied-UID groups retain bounded read compatibility only: after generator removal a sole survivor can fall back to its stored series UID, or multiple survivors can be ambiguous. No bulk migration is performed. Mixed DATE/date-time and equal native start/due values remain a separate strict RFC5545 fidelity gap; no timestamp/precision guessing is included.
