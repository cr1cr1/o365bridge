# Suspended account showed empty folders on IMAP instead of an error

Status: fixed (2026-10-25).

A suspended consumer account answers write calls (send, store) with an
explicit error (`ErrorAccountSuspend*`), but read listings with plain
success and zero items. IMAP therefore reported empty folders and
oscillating counts while SMTP alone failed with 451.

Fix: the registry records account-level degradation whenever an
upstream call fails with `ErrorAccountSuspend`. While the signal is
fresh (60 s), IMAP commands fail fast with
`NO ... account suspended upstream: verify the account in Outlook on
the web, then retry`, and SMTP returns 451 carrying the same reason
instead of a generic temporary failure. After 60 s the next command
goes through as a probe: a failure re-arms the block, and any
successful call clears the state, so a verified account recovers
within one interval without a bridge restart.
