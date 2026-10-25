# Deleted messages reappeared after reload

Status: fixed (2026-10-25).

A client that marks messages `\Deleted` (Thunderbird shift+delete with
default settings) saw them disappear locally and return on the next
folder reload: the mark lived only in the connection's memory, and
Exchange has no `\Deleted` flag, so nothing was stored or deleted
upstream.

Fix: marking `\Deleted` now acts immediately and is not
Thunderbird-specific. The marked messages move to the account's
Deleted Items folder upstream, then leave the selected view with
untagged EXPUNGE responses. EXPUNGE deletes permanently upstream
(OWA `HardDelete`) instead of moving to trash. Marking inside the
trash folder itself stays session-local so a following EXPUNGE removes
those messages for good. If the upstream move fails, the mark stays
session-local and the next EXPUNGE retries the removal.

Unmarking a message after the move cannot restore it: the message is
in Deleted Items, recoverable like any deleted mail.
