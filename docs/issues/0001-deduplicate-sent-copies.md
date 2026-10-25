# Sent copies duplicated on send

Status: fixed (2026-10-25).

Sending through the bridge SMTP stored the sent message twice in Sent
Items: the upstream send itself saves a copy (OWA `SendAndSaveCopy`,
Graph `/me/sendMail`), and clients that "place a copy in Sent"
(Thunderbird's default) append the same message again over IMAP.

Fix: message listings now request the Message-ID (OWA
`message:InternetMessageId` additional property, verified live; Graph
`internetMessageId` select). An IMAP APPEND to the account's Sent
folder compares the appended Message-ID against the newest page of the
folder, where the send's copy is the newest item, and answers OK
without storing when it is already there. Clients append within
milliseconds of the send while the upstream listing can lag or
hiccup, so the lookup retries briefly (three attempts, 400 ms apart)
before falling back to storing. Stateless: no map, no timeout; it
works across restarts and at any later time. When the folder cannot
be listed, the copy is stored as before. Deleting the visible copy
deletes the sent mail itself, as with Gmail.

Update (2026-10-25): the same stateless rule now covers JMAP. A
`Email/set` move into a Sent-equivalent mailbox checks the Sent
folder for the Message-ID first; on a hit it drops the source copy
instead of moving. The check lives in one shared helper
(`bridge.FolderHasMessageID`), so IMAP APPEND and JMAP use the
same retry and the same matching.
