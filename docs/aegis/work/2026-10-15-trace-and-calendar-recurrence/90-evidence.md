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

## Remaining work

Native recurring calendar diagnosis/repair, final independent review and whole-tree verification are pending. Live Outlook/Thunderbird acceptance is not established.
