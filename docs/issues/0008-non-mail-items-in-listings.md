# Empty messages in trash were calendar and task items

Status: fixed (2026-10-25).

The trash folder (and other generic folders) holds deleted items of
every type: calendar entries, tasks, contacts. The OWA mail listing
returned them all, and the bridge showed each as an empty message
(no subject, no sender, no usable MIME body).

Fix: the OWA FindItem row parser skips rows whose item type or
item class is not mail (`CalendarItem`, `TaskItem`, `ContactItem`,
`DistributionListItem`; `IPM.Appointment`, `IPM.Task`,
`IPM.Contact`, `IPM.DistList`, `IPM.Activity`). Meeting requests
and posts stay, because they are real mail with MIME. Verified
live on the enterprise account: INBOX still lists 345 mails; the
trash, whose 234 items are all calendar/task debris, now lists 0
mails in IMAP while the items remain in the OWA web trash.
