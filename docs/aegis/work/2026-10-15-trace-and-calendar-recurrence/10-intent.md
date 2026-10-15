# Contextual trace logging and native calendar recurrence

## TaskStartSnapshot / authority

- Direct owner request, in this exact execution order: (1) contextual IMAP command trace, (2) upstream sent/received payload trace, (3) investigate recurring events missing in Outlook cloud.
- Branch `main`, starting HEAD `e14dcfd`; clean starting worktree. Date 2026-10-15.
- Existing design owns the thin translator, per-account isolation, native OWA/Graph providers and no local content store. The new explicit payload-trace request supersedes the historical ban on bodies at trace only; credentials/session material remain excluded.
- Parent: direct bounded request plus the existing o365bridge design. No separate implementation plan or architecture replacement.

## Change necessity and scope

IMAP command logs currently expose only cmd/auth at debug. Upstream summaries omit payloads needed to diagnose native schema problems. Recurring cloud-event creation needs diagnosis at the DAV-to-provider boundary rather than speculative changes to readback.

Medium risk: trace reveals personal content and recurrence writes can lose data. ArchitectureReviewRequired: no new architecture; review logging/security boundaries and native calendar schema changes independently. Do not alter task recurrence, account selection, auth/fallback policies, or event identity/exception semantics unless direct evidence requires a scoped correction.

## Slice contracts / success evidence

1. IMAP: trace-only command record includes account, remote, tag, command (including UID), encryption and selected-mailbox context. No raw arguments, credentials, authentication literals or message contents. Existing warning/info behavior stays intact. Actual IMAP socket regressions verify context and debug exclusion.
2. Upstream: trace-only request and response payloads for both OWA and Graph provider transports, including failures, with account/operation correlation. Never log auth headers, cookies, tokens or browser-login traffic. Preserve protocol bytes, errors, streaming limits and zero extra body work when trace is disabled. Bound unusually large records and label any truncation. Document the sensitive-content risk and trace invocation.
3. Calendar: establish concrete native create/update/readback failure, write HTTP-backed regression evidence, make the smallest owner-level fix. Unsupported recurrence must fail visibly rather than be silently misrepresented. No claim of real Outlook/Thunderbird acceptance without a live session.

## BaselineReadSet / usage

Acknowledged: README calendar guidance; base design intent, anti-goals and logging constraints; IMAP dispatch/session/auth and logger configuration. No repository AGENTS/CONTEXT files discovered. Read additional provider/codec baselines only when their ordered slice starts.

## Recurrence diagnosis decisions

Confirmed fixes stay at existing owners: native event converters/readers, explicit repeat-mutation flags, and DAV full representations. Task pattern parsing is reused without changing task semantics; event anchor/cutoff, clamping and interval constraints remain separate. The shared pure RRULE comparator is renamed only. Canonical Windows readback is corrected from alphabetical aliases to pinned CLDR global defaults after regressions demonstrated Eastern/Beirut DST errors; existing forward aliases remain approximate.

Independent review established that Graph calendarView cannot own whole-series resources: its contract returns expanded occurrences/exceptions, while this event model has no recurrence-ID. Native `/events` listing is the single canonical Graph read owner for single events and series masters. Single-event overlap filtering remains bounded by the requested window; masters are retained for client RRULE expansion, not expanded or given fabricated occurrence IDs.

Anti-Entropy Declaration: code-retirement of an internal read responsibility, not live-state deletion. Old path: Graph ListEvents using expanded calendarView rows as whole-series objects. Remaining legitimate calendar operations (free/busy, native create/update/delete) stay at their existing owners. New owner: native event-object listing. Preserve native IDs/UIDs, singles, paging and account auth; retire only flattened occurrence listing as the canonical series source. External boundary: HTTP reads of the same account/calendar, no new permissions or data mutation. Source-of-truth data risk: none; no persistent-state migration/deletion and no user data erased. User confirmation required: no for this internal translator correction authorized by the recurrence repair request. Retirement decision: delete-first, no dual-source fallback. Verification: actual HTTP fixtures assert `/events` and forbid calendarView; native master/default-field readback, single overlap, native repeat edits/clear, and no upstream writes during list.

Graph response read normalization accepts documented irrelevant defaults and range sentinels, separately from strict write validation. PUT ETags for series are omitted until canonical GET rather than adding potentially failing post-write reads. Mixed invitation replies and repeat changes are rejected before any response mutation. Unsupported clamping/exception semantics remain explicit limits; no general recurrence engine or UID rewrite is introduced.

## Verification / stop

Verify each slice before the next; scoped commits in requested order. Final independent connected-code review, race tests, vet, build `bin/o365bridge`, diff check and clean tree. Done means code delivery with evidence and explicit live acceptance limitations. Needs-verification means native behavior cannot be reproduced locally; do not invent a fix or claim the incident resolved.
