# Task detail evidence and delivery boundaries

## EvidenceBundleDraft

Baseline: `214212d`; scoped task detail change on main. No live account credentials or cloud data were accessed. Native wire schemas were checked against public Microsoft Graph APIs and Exchange DataContracts; those are static compatibility evidence, not tenant acceptance.

Final parent verification:
- `go test -race ./... -count=1` — pass, all root packages, including SMTP in this run (job bash-1140, exit 0).
- `go test -count=5 -race ./internal/bridge ./internal/davserver ./internal/owa ./internal/graph ./internal/jmapadapter` — pass (job bash-1141, exit 0).
- `go vet ./...`, `go build -o bin/o365bridge .`, `git diff --check` — pass in the same chained verification.
- Independent read-only review: all concrete blockers corrected; final Ship verdict. The exact RRULE-ordering regression was run and passed independently. Prior focused tests also passed for conditional task writes, cutoff precision, native-detail preservation and positive-offset anchors.

## Covered behavior

- Existing date/time parsing plus preserved client DATE precision in native metadata after restart; explicit full-update clearing and partial-update preservation; invalid dates reject instead of vanish.
- Task category names, including escaped punctuation/Unicode, native arrays and VTODO CATEGORIES.
- Plain notes from authoritative TextBody including empty, HTML visible-text fallback, safe Graph HTML writing, and preserved unchanged rich formatting across CRLF/LF projection. Hidden script/style/head markup is excluded without Unicode byte-offset errors.
- Native task-specific scheduled repeat schemas, weekday/month/year selectors and count/end ranges, strict no-loss validation, separate repeat date/timezone, no invented task start date, valid DATE versus DATE-TIME UNTIL projection, and unchanged-rule preservation even when client ordering/defaults differ.
- Unsupported regeneration/relative subsets/ambiguous offsets/exceptions remain preserved or are rejected; all timing candidates and nanoseconds are checked before converting timed UNTIL to inclusive native end dates.
- Files listed as metadata, authenticated bridge ATTACH URLs, bounded on-demand GET/HEAD, exact account/list/task/file ownership and registry-retry revalidation, opaque IDs, MIME/disposition/nosniff/no-store, zero-byte files and error/size limits. Unsupported external/binary upload/removal references reject before mutation.
- Ordinary edits preserve reminders, exact native progress/completion metadata, files and unsupported repeat. Stale/weak/missing-resource conditional Task PUTs reject before update/create.
- Actual OWA adapter + fake HTTP -> actual DAV handler covers GET/query/absolute multiget, native file GET/HEAD, notes/categories/repeat/DATE output, ordinary title edit preservation, fresh handler readback, and no writes on unsupported attachment input.

## Residual limitations / uncovered acceptance

- Native file upload/delete is not implemented; file attachments are read/download only. Embedded items/reference/source links are not exported as file bytes, and arbitrary URLs are never fetched. A client opening an ATTACH link may separately request bridge authentication.
- VTODO alarm editing and full native status/progress representation remain limited; ordinary edits preserve those fields.
- Live Outlook/Thunderbird due-date normalization and timezone display, actual recurring occurrence expansion (especially month-end/leap-day clamp versus RFC skip), regeneration advancement and attachment-open authentication require owner-run acceptance. Basic due/note support existed before; no universal live missing-date cause is claimed.
- Graph task content and DATE metadata are updated in separate non-atomic requests. Failures propagate and retry addresses the same task rather than recreating it.
- Clients must synchronize after restart before editing old cached objects. If-Match guards stale conditional writes but is not an atomic upstream compare-and-swap; unconditional full PUTs still require current data.

The previously reproduced intermittent SMTP shutdown-test failure remains a known baseline issue, but did not occur in this final full-suite run; SMTP sources were not changed.
