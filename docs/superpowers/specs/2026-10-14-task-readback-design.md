# Outlook task readback and CalDAV identity

## Intent and boundary

Repair task creation and viewing without representing Outlook tasks as appointments. CalDAV task collections advertise and serve `VTODO`; event collections advertise and serve `VEVENT`. OWA uses native Exchange `Task` items in task folders; opt-in Graph uses Microsoft To Do tasks.

TaskStartSnapshot: clean working tree at `1ad7738`. Change Necessity: production adapters do not preserve a client task UID and OWA maps sparse mail-shaped discovery rows without task hydration. Existing fake-provider tests conceal these defects. ArchitectureReviewRequired: yes, narrowly for durable task identity metadata and collection type enforcement; no new store or auth channel.

## Evidence and chosen repair

- OWA `ListTasks` currently uses `FindItem` with `IdOnly`/`MailListItem` and treats results as complete tasks. Discover IDs with task-neutral paging, then hydrate task fields in bounded `GetItem` batches. An incomplete read must fail, not look like deleted or empty tasks.
- Outlook tasks do not have the calendar item's native iCalendar UID property. Preserve bridge-created client UIDs in supported task metadata. For existing tasks without that metadata, use the unique cloud task ID consistently in the href and serialized `UID`. No title matching and no local task-content cache.
- A successful creation must return a cloud ID and subsequent reads must resolve the same UID. Repeated PUT updates the selected task instead of adding a duplicate. Identity must survive adapter/registry reconstruction.
- PUT must validate collection existence and component support before mutation. Reject VTODO in event collections, VEVENT in task collections, and mixed/multiple objects. Keep existing appointment behavior unchanged.
- Task edits use native task fields, including completion/reopening and supported cleared fields; do not send calendar items or convert due dates into appointment times.
- Keep OWA's existing distinguished `/tasks/` collection stable. Do not replace it with opaque default-folder URLs or advertise the same primary folder twice. Custom OWA task-list enumeration is outside this repair; Graph continues to expose discovered To Do lists.
- OWA named UID metadata uses `PublicStrings` / `O365BridgeTaskUID` / `String`, with the equivalent PublicStrings GUID accepted on read. Graph creates `com.o365bridge.task` open-extension metadata atomically in the initial task POST.
- OWA READ GetItem fields use EWS-style URIs, while UPDATE paths use native JSON names such as `TaskDueDate` and `TaskPercentComplete`. Check all native response messages for error classes/codes even when MessageText is absent; HTTP 200 alone does not prove an upstream task update succeeded.

## Acceptance evidence

1. Discovery advertises a separate VTODO collection.
2. Real OWA adapter against fake HTTP: sparse discovery yields complete independent tasks, including undated tasks and same-title tasks.
3. Task PUT 201 -> GET 200 -> query/multiget -> repeated PUT 204 -> completion/reopening -> DELETE 204 -> GET 404, with native task requests and no appointment creation. A fresh handler/registry can resolve the created UID.
4. Discovery follows pages beyond 1,000; hydration batches are at most 100. Missing IDs/items or response-message failures return errors, not partial successes.
5. ID fallback is nonblank in task iCalendar, opaque IDs round-trip, and selective update/delete do not touch a same-title sibling.
6. Component mismatch/unknown collections reject writes without upstream task/event mutation.
7. Existing calendar and neighboring provider/DAV tests, race checks, vet, and build continue to pass, subject to the independently established intermittent SMTP shutdown baseline failure.

## Explicit limits

No expansion into Microsoft Planner, flagged-mail synthesis, custom OWA task-list enumeration, advanced recurring-task/exception semantics, or a new authentication flow. Resource URLs remain UID-derived; arbitrary client filenames differing from the component UID retain the existing identity-policy limitation. Fake HTTP proves covered request/translation behavior, not acceptance by a live tenant. Microsoft service metadata support must be checked against documented/native schemas; unsupported metadata must not be silently replaced by calendar fields or title-based identity.

## Verification

The scoped native task repair is implemented. Independent review found and closed three issues before final verification: preserve the existing `/tasks/` collection URL, accept a normalized PublicStrings GUID on UID readback, and reject native error-code-only response messages instead of returning false update/delete success.

Covered acceptance evidence includes native task creation with a client UID, fresh handler/registry readback after metadata normalization, undated legacy task cloud-ID fallback, separate VTODO discovery, query/absolute multiget, repeated PUT without duplicate creation, completion/reopening, selective deletion, component guards, and task fields after a nested alarm. Task discovery regression covers 1,001 records and GetItem batches no larger than 100. OWA readback error tests cover missing/duplicate/unexpected IDs, wrong item types, missing pages/items, paging non-progress, and per-message native errors. Graph tests cover atomic UID extension creation, fresh-provider readback, native cloud-ID fallback, same-title independent identities, ordinary updates leaving UID metadata intact, deletion, and missing create IDs.

Final verification: `go test -race ./... -count=1`, ten race repetitions of DAV/OWA/Graph/JMAP adapters, `go vet ./...`, build of the bridge executable, and `git diff --check` pass. Live Microsoft/Thunderbird acceptance still requires the owner to restart the rebuilt bridge and synchronize the remote Tasks collection. The previously established intermittent SMTP shutdown baseline issue was not modified by this task repair.
