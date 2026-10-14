# Native task detail synchronization — design and intent

## TaskIntentDraft

The user reports missing task due dates, repetition, categories, attachments, and notes in Thunderbird. Repair cloud-to-CalDAV representation and supported native updates without converting tasks into appointments or erasing unsupported metadata.

TaskStartSnapshot: main at `214212d`, initially clean. Parent authority: direct task-detail request; native goal `goal-6f7b20ef-7d90-4fb5-b2ae-f61661c69f89`. No implementation plan artifact; implement in small owner-scoped slices. This directory is the single durable process/design carrier.

## BaselineReadSetHint / BaselineUsageDraft

Read the shared Task/Provider model, OWA task hydration/update/recurrence helpers, Graph task model/serializer, and DAV VTODO codec/routes. Earlier task and event-category design sections are retained as baseline. Basic dates and plain notes already have mappings; do not claim every missing live date/note has a proven cause. Proven omissions: task categories/attachments, Graph repetition, OWA repetition updates and lossy weekday/monthly/yearly conversion, date-only flags, and plain-note handling of HTML/authoritative empty text. No live tenant payload or Thunderbird session is available.

## ImpactStatementDraft / chosen contracts

- Native tasks remain VTODO. Extend the shared Task with categories, date-only flags, explicit full-update/repeat presence, native repeat range anchor/timezone, unsupported-repeat preservation, unchanged-note preservation, and file attachment metadata.
- A full VTODO PUT may clear supported omitted fields, while generic partial task updates retain unsupplied fields. Do not clear unsupported native repeat or rich HTML notes on unrelated edits. Honor task If-Match to reject stale cached detail-loss writes before mutation.
- Repetition uses task-specific strict native codecs, not the lossy calendar helpers. Support faithful daily/weekly/month-date/single-relative-weekday/year-date patterns and bounded recurrence ranges. Unsupported native completion-relative or unrepresentable patterns remain preserved, not invented as monthly schedules. Reject unsupported incoming repeat changes before writing.
- Do not invent a native task start date from a repeat range anchor. Preserve date-only VTODO semantics. Do not use unverified timezone/noon heuristics for cloud dates. Invalid nonempty dates fail safely rather than vanish into nil.
- Notes prefer present TextBody, including empty. HTML-only native notes convert to plain DESCRIPTION through a shared helper; unchanged plain text skips a native body rewrite to preserve rich formatting. Graph note writes use escaped HTML per the documented API restriction; OWA uses native Text bodies.
- Task categories use native arrays and escaped CATEGORIES; names only, not Outlook color catalogs.
- File attachments are metadata plus on-demand bytes, not a local store. Optional TaskAttachmentProvider downloads only a file that belongs to the selected task in the selected list. DAV ATTACH exposes an authenticated, credential-free bridge URI; never cloud tokens/canaries or arbitrary server-fetched external URLs.
- Attachment GET/HEAD is account/list/task/attachment scoped, escaped once, bounded to 32 MiB decoded bytes, attachment-disposition, nosniff, private/no-store. Revalidate membership with the active provider during registry retries/fallbacks. Existing native files survive ordinary task edits; new/changed external/binary attachment inputs are explicitly rejected instead of claiming upload. Native file upload/delete and embedded/reference-item export are outside this read/download slice and must be reported as unsupported, not silently synchronized.

## Execution Readiness View

OWA metadata/detail/repeat owner: calendar task methods and task-specific recurrence tests. OWA attachment owner: new attachment helper/download tests and narrowly bounded native transport. Graph owner: task fields/repeat/attachment tests and narrowly bounded raw transport. Parent: shared model/note helper, DAV codec/routes/security/full-update policy, HTTP-backed lifecycle integration, documentation, independent review, verification, build, commit. No event/calendar recurrence changes, SMTP fixes, new auth channel, file cache, or general upload framework.

## Success evidence / stop states

Real adapter + fake HTTP should expose date-only due, exact note/category names, faithful repeat, and authenticated file attachments through GET/query/multiget. Supported edits/clear and ordinary unrelated edits preserve native file/rich-note/unsupported-repeat metadata; unsafe attachment/repeat changes fail before writes. Tests cover cross-account/task isolation, opaque IDs, malformed/bounded binary content, zero-byte files, native response errors, strong ETags, stable UID across restart, and preserved sibling tasks. Require scoped race repetitions, full-suite attempt with the established SMTP shutdown flake reported honestly, vet/build/diff check, and independent review. Live Microsoft/Thunderbird acceptance remains owner-run.

Stop states: done, needs-verification, or concrete external blocker; no silent scope expansion into attachment upload or invented recurrence semantics.
