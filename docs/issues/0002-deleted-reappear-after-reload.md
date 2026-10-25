# Deleted messages reappeared after reload

Status: fixed (2026-10-25).

A client that marks messages `\Deleted` (Thunderbird shift+delete with
default settings) saw them disappear locally and return on the next
folder reload: the mark lived only in the connection's memory, and
Exchange has no `\Deleted` flag, so nothing was stored or deleted
upstream.

Fix: the `\Deleted` mark is the permanent-delete gesture and acts
immediately, not Thunderbird-specific. Clients send their own
`UID MOVE` to the trash folder for regular deletes; a mark with no
following client move means "remove for good". Marked messages are
deleted permanently upstream (OWA `HardDelete`) in every folder,
then leave the selected view with untagged EXPUNGE responses.
EXPUNGE also removes permanently. If the upstream delete fails, the
mark stays session-local and the next EXPUNGE retries the removal.

Unmarking a message after the move cannot restore it: the message is
in Deleted Items, recoverable like any deleted mail.
