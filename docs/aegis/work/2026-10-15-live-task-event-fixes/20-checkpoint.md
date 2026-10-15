# Checkpoint

## Completed slice

`5cdf4e4`: shared quoted DAV parameter parser, task/event/contact regressions and native OWA plain-body checks. Parent full race suite, five affected repetitions, vet, dist build and independent review passed before commit. Live user writes at11:22 (UpdateItem134/CreateItem150) confirm clean native notes and clean readback.

## Current slice

Native recurring-task identity repair is implemented and in final verification/review; not yet committed. User reports update+sync removes local task and doubles Outlook rows. Native split semantics and copied UID collision were captured, then authoritative flags confirmed by owner restart/sync at11:39: continuing master IsRecurring=true/TaskDeadOccurrence=false, detached sibling IsRecurring absent/TaskDeadOccurrence=true. Both retain Recurrence and incomplete statuses; no completion/date/title ordering heuristic is used.

Provider behavior:
- Request native IsRecurring plus PSETID_Task Boolean33033; validate namespace/type/presence strictly.
- Explicitly dead rows expose no active recurrence, while sole final tasks retain stored UID.
- Historical repeated-UID groups resolve only with one proven generator and known-dead siblings; retain every native row. Ambiguity/introduced UID collisions fail closed. Reads never mutate cloud metadata.
- Future successful split responses with a changed native ID trigger one UID-only metadata assignment to that detached ID. This is a user-write follow-up, not bulk cleanup. It preserves future detached identity through finalization/deletion/reconstruction.
- Post-write repair failures carry outer KindOther preserving cause, preventing Registry.Call auth recovery from replaying an already-applied native date edit. Initial-call auth recovery remains intact.
- Task recurring create/update/clear PUTs omit predicted ETags; GET supplies the next canonical validator. Ordinary task validators remain unchanged.

## Acceptance evidence

Parent native HTTP/DAV regression first reproduced wrong detached-first task lookup at the original href, then passed: continuing series href, distinct one-off href without RRULE/private anchor, complete query, selective multiget, provider/handler reconstruction, explicit test-series deletion without one-off rename. Parent job bash-77 exit0 also covers recurring-task ETag cases with ordinary/conditional-edit controls.

Provider tests cover exact flags/UNKNOWN, paging/order/reconstruction, single final/standalone UID, 12 ambiguity/collision cases, UID-only follow-up and lifecycle. Real HTTP401 + Registry.Call red showed three native edits after post-write auth failure; green proves one edit, no renewal/replay, and a safe initial-auth positive control. Child bash-82 exit0.

Parent FINAL gate job bash-84 collected exit0: full race suite, five repeated DAV/OWA race suites, vet, dist build and diff check. Independent reviewer348ee7c3 reviewed the final real401 fixture/post-write guard and gave Ship with no remaining concrete must-/should-fix. Documentation feedback reconciled read/write scope and legacy sole-survivor fallback. All subagents inactive; all relevant parent background jobs collected.

## Remaining boundary / next

Commit the independently verified native identity/retry/ETag slice as a bounded correction, NOT full client symptom closure. Parent final gate bash84 and final independent review both passed. Additional user commit1a82009 changes only mise-tasks/run.sh; preserve it untouched.

Latest owner test restarted at12:40:56: existing cloud tasks still absent locally; new standalone taskcreate10 and noteUpdate23 succeeded and remain visible. Repeated client multiget requested_hrefs2/returned0, while parent read-only canonical query/multiget130 had4uniquehrefs/UIDs and1responseeach. Actual requested client hrefs are not logged, so rawslash/encoding/stale-URI hypotheses remain unproved. Reviewer348ee7c3 investigates read-path/instrumentation read-only.

New recurring conversion request42 fails natively at12:41:37: ErrorInvalidPropertyRequest MessageXml explicitly says calendar:Recurrence on Task. Provider393c1c22 preparing strict native task SET/DELETE path regression; source changes are held until the verified identity slice is committed. Expected task-only TaskRecurrence update path, not creationJSONfield or calendar-event path. Do not guess the missing-href cause from counts alone. No live appointment-event writes have yet appeared.

No cloud rows are automatically deleted/merged. Historical copied-UID projection is not durable once its generator ends/deletes: multiple survivors can be ambiguous, a sole survivor can return to its stored series UID. Future write-assigned detached UIDs avoid that boundary. Partial native edit/UID assignment is non-atomic and failures surface. Mixed DATE/date-time or equal native start/due remains an independent strict iCalendar fidelity gap; no lossy date coercion is included.

## Resume

Read intent, this checkpoint, main design task section and final evidence when present; compare main HEAD/worktree before continuing. Initial HEADebcd78a; current committed HEAD5cdf4e4. Goal goal-84f0d2fb-0154-49d3-99e0-4f1edd21256c revision1 active. Log rotates/truncates on restart (latest inspected header11:39:10); inner ANSI severity is authoritative, not wrapper stderr level:error. Partial lines during append are not by themselves corruption. Process-list absence did not prove shutdown despite fresh polling.

Agent has performed no direct live API mutation, cloud deletion, content cleanup or process-compose configuration change. Code now performs the narrowly described UID metadata follow-up during future user-initiated writes.
