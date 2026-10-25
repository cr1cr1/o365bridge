# Sent copies duplicated on send

Status: fixed (2026-10-25).

Sending through the bridge SMTP stored the sent message twice in Sent
Items: the upstream send itself saves a copy (OWA `SendAndSaveCopy`,
Graph `/me/sendMail`), and clients that "place a copy in Sent"
(Thunderbird's default) append the same message again over IMAP.

Fix: the SMTP path records the sent Message-ID for 10 minutes; an IMAP
APPEND to the account's Sent folder within that window maps to the
already-stored copy instead of creating a second one. Deleting the
visible copy deletes the sent mail itself, as with Gmail.
