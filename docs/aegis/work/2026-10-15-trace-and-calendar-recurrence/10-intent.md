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

## Verification / stop

Verify each slice before the next; scoped commits in requested order. Final independent connected-code review, race tests, vet, build `bin/o365bridge`, diff check and clean tree. Done means code delivery with evidence and explicit live acceptance limitations. Needs-verification means native behavior cannot be reproduced locally; do not invent a fix or claim the incident resolved.
