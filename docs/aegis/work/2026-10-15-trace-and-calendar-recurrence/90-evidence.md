# Verification evidence

## Slice 1 — contextual IMAP command trace

- Existing socket-based tests extended to cover trace vs debug, pre/post-login account context, remote address/tag, UID compound commands, selected/UNSELECT/read-only state, plaintext and STARTTLS.
- Regression observed before dispatch change: missing tag/context, and command record incorrectly visible at debug.
- Parent verified: `go test -race ./internal/imapserver -count=3`, `go vet ./internal/imapserver`, `git diff --check` — all exit 0.
- Parent review: dispatch-only trace fields; no command arguments, passwords, SASL responses, search arguments or APPEND literal contents. Existing authentication/failure severity remains intact. No new architecture/dependency.
- Live Outlook/Thunderbird acceptance not performed; unnecessary for the local IMAP logging contract.

## Slice 2 — safe upstream payload trace

- OWA/Graph trace-only request/response pairs include channel/account, method/action/resource, request ID, direction/status and sanitized JSON. Existing summaries stay at debug/warn. No browser/token-exchange tracing.
- Capture at most 64 KiB; render at most 48 KiB sanitized JSON. Unsafe incomplete/malformed/over-cap and opaque MIME/binary payloads, including native JSON-wrapped MIME/file bytes, are explicitly omitted. No raw unsafe prefixes. Trace remains sensitive personal-content output, not anonymization.
- Real adapter tests cover unchanged wire/auth bytes, native/HTTP/malformed/empty/error/partial responses, nil/debug exclusion, concurrent correlation, cancellation/network failures, request/response secrets and bounds. OWA tests prove identical reads in nil/debug/trace modes, no drain, and unchanged known-length/limited-reader attachment guards; Graph tests retain existing bounded-download and error behavior.
- Independent reviewer found and worker corrected reversible MIME inside JSON and mixed/lowercase URL-encoded secret gaps; parent confirmed/corrected malformed-percent fail-open and generic/scheme-relative URL userinfo. Dedicated helper/real OWA MIME regressions verify these cases, plus output-escaping expansion.
- Existing calendar count-summary privacy test now explicitly uses DebugLevel, retaining every count/content assertion; new explicit trace behavior supersedes its historical implicit trace assumption.
- Parent fresh final verification after all corrections: `go test -race ./internal/upstreamlog ./internal/owa ./internal/graph -count=3`, `go vet ./internal/upstreamlog ./internal/owa ./internal/graph`, `git diff --check` — all exit 0.
- Reviewer found no remaining transport/IMAP must-fix and conditioned ship on the malformed-percent regression, now verified. Reviewer test-cache sandbox restriction did not prevent parent verification in the configured project cache.

## Slice 3 — native recurring calendar creation/readback

- Root regressions: OWA omitted required interval/selectors and ignored repeat edits, Graph omitted native recurrence, and both could accept success-shaped create responses without a native ID. Native HTTP schema tests observed the original failure before corrected requests.
- OWA and Graph use thin event-specific date/cutoff wrappers around existing pure scheduled-pattern converters. Local start dates, UTC timed UNTIL vs DATE all-day UNTIL, inclusive cutoff before/exact/after local start, DST transitions, weekdays/month selectors, count/end/no-end, explicit set/clear and native-ID gates are covered. Task converter implementations are unchanged.
- Actual OWA HTTP + DAV regression covers recurring native create acceptance, fresh GET/query/multiget, zero repeat writes on ordinary title edit, explicit replacement and clearing. Graph HTTP tests cover typed native read/write, unsupported patterns/zones, native defaults/sentinels, sparse response merging and required ID; no same-path Graph initial-client-UID parity claim.
- Independent review found/corrected: (a) invitation response bypass before invalid repeat mutation, (b) EWS absolute day-clamping vs RFC skip semantics, (c) immediately stale PUT ETag after native selector defaults, (d) redundant original-rule default/ordinal rewrites, (e) Graph expanded occurrence listing as wrong canonical master owner, and (f) valid Graph response defaults/zero-date sentinels rejected as unsupported.
- Mixed repeat+invitation edits now reject before RespondEvent; valid ordinary zoned invitation responses remain covered. Series PUT ETags are omitted until canonical GET, including clearing. Known unrepresentable native rules survive unrelated edits and unsafe replacement/movement is refused.
- Clamping-sensitive absolute monthly days 29–31 and yearly leap/invalid dates are event-only unsupported; OWA documented interval maxima are tested at zero/one/max/max+1 on create/update/read. Root metadata absent-only compatibility remains strict about actual polymorphic pattern/range types; no native capture established whether this tenant omits it.
- Shared reverse Windows-zone lookup now uses 31 pinned CLDR global canonical corrections after DST regressions exposed Eastern->Atikokan and MiddleEast->Amman errors. Every existing supported ID is deterministic/loadable; forward aliases/legacy fallback/UTC spelling stay unchanged and approximate. Reference: CLDR commit `2ef784e3a4168bc2a43cd1b5b9839b6636f5899c`.
- Graph native `/events` is the sole series-object read source; negative HTTP fixtures refuse calendarView. Full Microsoft list/create JSON samples, custom/default calendars, two-page reads and single-event overlap/zero bounds are covered. Masters are conservatively retained, not occurrence-window-expanded; meaningful invalid native data stays unsupported. No dual-source fallback or cloud-data migration.
- Final independent connected-code review: **Ship; no remaining concrete must-fix/should-fix in the requested slice**, subject to parent final validation. Reviewer verified final Graph corrections and all prior fixes.

## Final parent verification (stable final source)

All commands below completed with exit 0 after the final Graph read-owner/default corrections:

```sh
export GOMODCACHE="$PWD/.gocache/mod" GOCACHE="$PWD/.gocache/build" GOPATH="$PWD/.gocache/gopath" GOSUMDB=off
go test -race ./... -count=1
go test -race ./internal/bridge ./internal/davserver ./internal/owa ./internal/graph ./internal/imapserver ./internal/jmapadapter ./internal/upstreamlog -count=5
go vet ./...
go build -o bin/o365bridge .
git diff --check
```

The final full race suite included SMTP and passed. Earlier concurrent development snapshots/new red tests are not terminal failures. Rebuilt binary is ignored by git and requires restarting the running bridge.

## Uncovered / intentionally retained limits

No live Microsoft/Thunderbird session or cloud credentials were used. Exact tenant visibility acceptance remains owner-run, not proven by schema fixtures. Native recurrence exceptions/shared UID/recurrence-ID identity, OWA later-window master expansion, conservative Graph master over-inclusion, initial CalDAV-client UID persistence on Graph, advanced JMAP repetition and generated/custom VTIMEZONE remain explicit limits. Clients require IANA tzdata and should refresh before edits; check Outlook before recreating items. Trace captures personal content and must be handled privately. No unrelated source/persistence migration, dependency or task-recurrence rewrite was added.
