# Checkpoint

## Committed delivery

| Commit | Verified correction |
|---|---|
| `5cdf4e4` | Quoted DAV parameters no longer leak ALTREP metadata into task/event notes. |
| `b1601a7` | Native continuing/detached task identities, durable new split UID assignment, no post-write authentication replay, canonical recurring-task GET validators. |
| `218ba6e` | Task recurrence SET/DELETE use TaskRecurrence rather than calendar Recurrence. |
| `566f5dd` | Thunderbird-normalized opaque resource URLs, explicit multiget missing statuses and safe ambiguity rejection. |
| `98cd0db` | Rejected DAV writes log account/op/status without private request contents. |

User-owned process-compose commit `1a82009` was preserved; no configuration edits or replacement server were made. Initial branch main/HEADebcd78a; current committed HEAD98cd0db. No direct live task mutation, cloud deletion or client data/cache cleanup has been performed by the agent. Only verified agent-created Go build-cache artifacts were removed.

## Live evidence already established

- Description update/create at11:22 produced clean native notes and matching readback.
- Native task date edit created a legitimate detached ID and advanced the original, copying its UID. Native flags then proved master versus detached ownership, not completion/title/date order.
- Read-only receipt195: all5canonical URLs worked but Mozilla normalization made2native slash IDs fail. After managed restart, receipt311: canonical5/5 and Mozilla5/5, distinct hrefs/UIDs, one response per resource. Actual client multiget13:42:54 requested2/returned2/missing0/dups0.
- TaskRecurrence SET220 succeeded against this tenant. Native split261 plus UID-only assignment262 both succeeded.
- Repeat removal initially had no provider write in the captured window, so no speculative native-delete change was made. Later14:43:37 request84 sent TaskRecurrence DELETE, returned NoError/DAV204, and native readback had no recurrence. This path is now confirmed.
- New standalone task create/edit stayed visible according to the user. Their latest explicit remaining cloud-update gap is the reminder. Exact percentages and attachment upload remain incomplete, not bundled.

## Current uncommitted reminder slice

Root: task VALARM was discarded by the codec and the handler always restored the old reminder even though providers already supported it. The implementation now maps one DISPLAY alarm to the existing native reminder instant:
- Absolute UTC triggers; signed W/D/H/M/S relative START/END only with explicitly zoned DTSTART/DUE. Resolve after all properties and before synthetic anchor cleanup.
- Civil day/week arithmetic before elapsed HMS; reject unresolved DST fold/gap anchors and nominal targets instead of guessing. Reject DATE/floating/unknown-zone anchors, task-DURATION fallback, repeated/multiple/non-DISPLAY or malformed alarms before native mutation. Event alarms unchanged.
- GET emits absolute UTC; fresh full PUT owns set/change/removal, including omission to clear. Conditional stale representations remain protected.
- Minimal transient ReminderUnchanged flag preserves exact old pointer on same-second equality and omits only reminder fields in OWA/Graph updates. Needed because restoring a 100ns pointer alone would still be truncated by millisecond formatting. Create/default partial/set/clear semantics unchanged; no new store/schema/auth channel.

## Fresh verification / review

Worker8282fd2e finished; production stable, scoped DAV/OWA/Graph suites and diff check passed, owned job356 collected. Parent native HTTP/DAV reminder lifecycle test351 collected exit0: create/read, native100ns note-edit preservation with zero reminder writes, relative END change, explicit full clear without stale DueBy resurrection, no recurrence writes/extra creation.

Parent FINAL reminder job357 collected exit0: full race suite, five repeated DAV/OWA/Graph race suites, vet, dist build, diff check. Independent reviewer952fc578 read final code/tests and gave Ship with no concrete must-/should-fix. README/main design describe supported alarm subset and precision/full-PUT semantics.

Managed restart360 exit0 deployed the build; listener health401 was intentional and made no task request. Owner selected “Reminder updates correctly.” In the final runtime window, native request21 enabled/set the reminder, NoError and matching readback25; request88 changed its instant, NoError and matching readback91. Intervening edits34/69 had no reminder fields and retained the value. Live reminder clear was not captured; native HTTP/DAV regression covers it. The reminder implementation/evidence are ready for the final scoped commit containing this checkpoint.

## Delivery state

Current task-reminder edits are verified locally and by owner/native live set/change readback. Finalize the scoped reminder commit and native session goal; no more runtime restart is required because the verified binary is already running. Further work requires fresh user intent or a newly reported failure, not speculative field expansion. No appointment-event write has appeared, so live appointment acceptance remains unclaimed. Exact percentages, attachment upload and broader alarm/snooze forms remain explicit limits.

## Resume / limits

Read intent, this checkpoint, current README task section and evidence; compare HEAD/worktree before continuing. Goal goal-84f0d2fb-0154-49d3-99e0-4f1edd21256c revision1 remains active. All relevant parent jobs collected; all subagents inactive at this checkpoint. User's process-compose controller/build/run is their existing runtime, not an assistant-managed background job to kill.

Logs rotate/truncate on restart; reread header before old offsets. Wrapper level:error classifies stderr, inner ANSI severity is the application level. Partial appends and capture-limit omissions are not by themselves native failures. A checker account-selection failure after rotation made no request/restart; subsequent managed restart341 and expected listener401 succeeded.

No cloud rows are automatically merged/deleted. Historical copied-UID projection is not a durable migration after generator removal; survivors can revert to stored UID or fail ambiguous. Native split plus UID assignment is non-atomic, with errors surfaced without edit replay. Mixed DATE/date-time/equal native task dates remain separate strict iCalendar fidelity limits. Custom alarm messages/snooze, exact progress, attachment upload, event alarms and ambiguous decoded collection-ID slashes are not claimed supported by this slice.
