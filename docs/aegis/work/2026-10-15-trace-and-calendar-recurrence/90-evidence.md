# Verification evidence

## Slice 1 — contextual IMAP command trace

- Existing socket-based tests extended to cover trace vs debug, pre/post-login account context, remote address/tag, UID compound commands, selected/UNSELECT/read-only state, plaintext and STARTTLS.
- Regression observed before dispatch change: missing tag/context, and command record incorrectly visible at debug.
- Parent verified: `go test -race ./internal/imapserver -count=3`, `go vet ./internal/imapserver`, `git diff --check` — all exit 0.
- Parent review: dispatch-only trace fields; no command arguments, passwords, SASL responses, search arguments or APPEND literal contents. Existing authentication/failure severity remains intact. No new architecture/dependency.
- Live Outlook/Thunderbird acceptance not performed; unnecessary for the local IMAP logging contract.

## Remaining work

Upstream payload tracing, native recurring calendar diagnosis/repair, final independent review and whole-tree verification are pending.
