# Deleted messages reappeared after reload

Status: fixed (2026-10-25).

A client that marks messages `\Deleted` (Thunderbird shift+delete with
default settings) saw them disappear locally and return on the next
folder reload: the mark lived only in the connection's memory, and
Exchange has no `\Deleted` flag, so nothing was stored or deleted
upstream.

Fix: marking `\Deleted` now acts immediately and is not
Thunderbird-specific. Outside the trash, the marked messages move to
the account's Deleted Items folder upstream; inside any folder marked
as trash or drafts, the marked messages are removed permanently
upstream (OWA `HardDelete`) — deleted mail would only move in a
circle, and a discarded draft is a working copy whose removal send
flows rely on. In both cases the messages then leave the selected view
with untagged EXPUNGE responses. EXPUNGE also removes permanently. If
the upstream call fails, the mark stays session-local and the next
EXPUNGE retries the removal.

Unmarking a message after the move cannot restore it: the message is
in Deleted Items, recoverable like any deleted mail.
