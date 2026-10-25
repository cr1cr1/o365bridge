# New messages were marked read on the first folder visit

Status: fixed (2026-10-25).

Opening the Inbox in the client marked all new messages as read, even
though Outlook on the web still showed them unread before the visit.

Cause: the FETCH handler treated every `RFC822.*` item as a read
event, including `RFC822.SIZE`, which is metadata only. Clients fetch
`UID ... RFC822.SIZE FLAGS BODY.PEEK[...]` during ordinary folder
sync, so the bridge set `\Seen` upstream for every newly listed
message.

Fix: only body fetches mark read: `BODY[...]` (non-PEEK), `RFC822`
(full message), and `RFC822.TEXT`. `RFC822.SIZE`, `RFC822.HEADER`,
`BODY.PEEK[...]`, ENVELOPE and FLAGS never do.
