# Live task/event diagnostics

## Intent and boundary

The user requested watching process-compose trace output during task/event create/edit and fixing encountered bugs, starting with description metadata rendered raw in Outlook. This is a direct implementation request, not a new architecture project. Keep the thin translator and existing per-account boundaries; do not change process-compose, delete cloud items, deduplicate by title, or migrate user content automatically.

## Task start / baseline

Workspace `/mnt/linux2/dev/o365bridge`, branch `main`, initial HEAD `ebcd78a` (user's process-compose addition), initially clean. Relevant baseline: README task details, main design task readback/identity sections, shared DAV parser and callers, native OWA task lifecycle. Initial low-risk parser work used inline state; promote that same workstream to this durable record because live testing revealed a native recurring-task identity boundary.

Description fix `5cdf4e4` is committed, independently reviewed and fully verified. Live task UpdateItem request134 and CreateItem request150 later confirm clean plain native Body and readback.

## Current anomaly / evidence

At 11:22:03 a task date edit returned a new native item ID while the old item advanced to the next day. Both retained the stored client UID and recurrence payload. DAV multiget returned two objects for one href (duplicate_uids=1). Both items are incomplete; do not assume one is completed history. Request134 did not change completion/status; it changed due date. Native due date earlier than start was normalized.

Microsoft documents that changing a scheduled recurring task's start/end creates a one-off task returned by UpdateItem while the original ID advances: https://learn.microsoft.com/en-us/exchange/client-developer/web-service-reference/updateitem-operation-task . The read shape originally omitted native state. It now requests IsRecurring and PSETID_Task Boolean TaskDeadOccurrence (0x8109). Fresh owner-run read-only synchronization at11:39 confirmed, for both duplicate groups: continuing master IsRecurring=true/DeadOccurrence=false; detached sibling IsRecurring absent/DeadOccurrence=true. Both retain recurrence data and incomplete statuses. The user independently confirmed that update then sync loses the local task and doubles Outlook rows.

Native instance authority: https://learn.microsoft.com/en-us/openspecs/exchange_server_protocols/ms-oxotask/c8c58ff9-d903-48fa-be47-e0a9287cc4fe and https://learn.microsoft.com/en-us/openspecs/exchange_server_protocols/ms-oxprops/dcf98464-a93f-485c-a0e9-fe86de91871b . Missing flags are unknown, not false. A sole final/dead task must not lose its stored UID. Only duplicate groups with a unique proven generator and known-dead siblings can be projected without ambiguity; all cloud rows remain intact.

A separate pre-existing mixed DATE/DATE-TIME VTODO projection limitation was identified during read-only review. Current evidence does not prove it caused the client's Oct15-to-Oct14 due change. Do not truncate/shift dates, infer omission from midnight, or claim identical redundant date writes generate instances: native docs only establish splitting for changed values. The identity repair must not smuggle in a speculative date-precision migration.

## Safety and verification

Change necessity: distinguish actual series from detached native items rather than treating copied metadata as identity authority. The implementation remains provider-local: bounded historical read projection plus one bridge-owned UID assignment when a successful native update returns a proven detached ID. The metadata-only assignment is part of the requested user write, never a read-time migration; post-write failures surface without registry replay. ArchitectureReviewRequired: no new store, schema, bridge model, auth channel or broad calendar identity layer; the existing native UID field keeps the same owner. Main design and README record this clarified read/write boundary.

Regression tests must use actual native split semantics and copied UID/recurrence data, not fake unchanged IDs. Preserve all cloud rows and normal nonrecurring client UIDs. Full race suite, repeated affected suites, vet, build, independent review, then scoped commit. Live appointment-event writes have not yet appeared; do not claim them verified.
