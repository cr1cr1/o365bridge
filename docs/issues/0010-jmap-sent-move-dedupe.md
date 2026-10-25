# JMAP move into Sent duplicated the send copy

Status: fixed (2026-10-25).

`EmailSubmission/set` sends through OWA `SendAndSaveCopy`, which
stores the Sent copy upstream. A JMAP client that then moves the
message into the Sent mailbox (`Email/set` `mailboxIds`, or
`onSuccessUpdateEmail`) produced a second copy: the move branch
ran `MoveMessages` unconditionally.

Fix: the dedupe rule that IMAP APPEND already used moved into one
shared helper, `bridge.FolderHasMessageID` (newest-page listing,
Message-ID match, brief retry for listing lag). The JMAP move
branch consults it when the destination is a Sent-equivalent
mailbox; on a hit it deletes the source copy instead of moving.
The IMAP APPEND path now calls the same helper, so the retry and
matching behaviour are defined once.
