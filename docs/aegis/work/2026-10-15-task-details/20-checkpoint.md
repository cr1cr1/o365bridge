# Task detail checkpoint

## TodoCheckpointDraft

Baseline: main `214212d`; no pre-existing working-tree edits. Diagnosis completed for five requested fields. All implementation slices are complete: native OWA/Graph task fields, task-specific strict repeat codecs, DATE metadata, shared plain-note helpers, file metadata and bounded downloads, authenticated DAV routes and VTODO serialization. Parent HTTP integration verifies date-only due, categories, repeat, notes, GET/query/absolute multiget, GET/HEAD file bytes, unchanged rich notes/reminders/progress, and rejection of unsupported attachment edits. Independent review findings were fixed; last helper-specific test is being checked. Final full race run bash-1140 and five-repeat affected race/vet/build/diff verification bash-1141 were collected, both exit 0. The independent exact-name RRULE normalization fixture also passed; final review has no remaining findings. See 90-evidence for covered behavior and limits.

## ResumeStateHint

Read 10-intent, this checkpoint, current native goal and git status, then the changed shared model and current provider/DAV task slices. Do not restart running subagent work or replace their files. Direct children: a39d84ef-0e1b-487b-82d3-26a2d9782aca (OWA details), 783920c0-af00-4ac0-bcab-8464318a890b (Graph), dd65f5d3-8d89-4542-a815-9aaef8677193 (OWA attachments). Parent will send settled schemas/contracts before final review. No completion claim; no live credentials or cloud data mutation occurred.

## DriftCheckDraft

Continue within task detail mapping and read/download attachment scope. Due/notes already work for basic fixtures; live missing-date diagnosis remains unconfirmed. Unsupported native repeat must be preserved; unsupported attachment uploads/removals must not return success as if synced. No changes to event recurrence, SMTP, auth recovery policy, or persistent task-content storage. Final status must disclose native/CalDAV limitations, not substitute successful unit parsing for live Thunderbird acceptance.

## Next smallest action

Verification/review complete, README/main design reconciled and evidence recorded. Commit the scoped task-detail change, then complete the native goal for this code-delivery scope and report read-only file/unsupported-repeat/live-acceptance limits. No implementation/test jobs or unresolved review findings remain.
