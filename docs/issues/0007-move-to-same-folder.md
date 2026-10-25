# MOVE to the same folder left the message upstream

Status: fixed (2026-10-25).

Deleting a message from the folder marked as trash in Thunderbird
sends `UID MOVE <uid> "Deleted Items"` while that folder is
selected. The bridge ran the upstream move anyway; Exchange
accepts a move into the same folder as a no-op, so the bridge
answered OK and dropped the message from the session view, and the
next SELECT fetched it again. This is not account-specific: it
happened for the enterprise account because that is where plain
Delete inside trash was used.

Fix: when a MOVE names the selected folder as destination, the
bridge treats it as the delete gesture and removes the message
upstream for good, then reports the expunge as before. Verified
live on the enterprise account with a disposable copy: before the
fix the copy stayed upstream after `UID MOVE` to the same folder;
after the fix it is gone.
