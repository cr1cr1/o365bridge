# Folder delete failed: mailbox does not exist

Status: fixed (2026-10-25).

Thunderbird deletes a folder by renaming it into the trash
(`RENAME folder "Deleted Items/folder"`). The bridge treated the whole
destination string as an existing mailbox name, answered
`NO mailbox does not exist: Deleted Items/folder`, and never moved
anything, so the folder stayed in place upstream.

Fix: RENAME now parses the destination as a parent path plus a new
leaf. When the parent differs from the folder's current parent, the
folder is moved upstream first (OWA `MoveFolder`, Graph
`/mailFolders/{id}/move`), then renamed when the leaf changed.
Verified live: renaming a folder into the trash path moves it there
upstream, where normal trash deletion applies.
