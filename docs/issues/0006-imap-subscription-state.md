# IMAP subscription state was not kept

Status: fixed (2026-10-25).

SUBSCRIBE and UNSUBSCRIBE returned OK but did nothing, and LSUB
listed every folder. So a client could not hide folders: after a
restart, every folder came back as subscribed.

Fix: the bridge keeps the unsubscribed folders only (default is
subscribed), keyed by upstream folder ID in `idmap.json` under
`unsubscribed`. SUBSCRIBE and UNSUBSCRIBE persist the change.
LSUB hides unsubscribed folders, but an unsubscribed parent stays
visible as `\Noselect` while a subscribed child exists, so the
tree still renders. LIST stays complete, per RFC 3501. Keying by
folder ID keeps the state across renames; deleting a folder drops
its entry.
