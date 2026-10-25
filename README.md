# o365bridge

- Personal, single-user Go daemon for one Linux desktop.
- For Outlook 365 tenants where IMAP/SMTP are disabled and Graph is not consented.
- Client protocols, all on loopback:
  - IMAP `1143`
  - SMTP `1025`
  - CalDAV/CardDAV `1080`
  - JMAP `18080` (0 disables it)
- Upstream: OWA by default; Microsoft Graph if a tenant admin supplies an app registration.
- No local mail store. Requests are translated live.
- Design: [main design](docs/superpowers/specs/2026-10-14-o365bridge-design.md)

## Quick start

```sh
go build -o o365bridge .
OB_PASSWORD='choose-a-bridge-password' ./o365bridge serve
```

Client settings (Thunderbird, Himalaya, etc.):

| Setting | Value |
|---|---|
| Username | Outlook 365 email address |
| Password | Bridge password (`OB_PASSWORD` above) |
| IMAP | `127.0.0.1:1143`, plaintext by default |
| SMTP | `127.0.0.1:1025`, plaintext by default |
| CalDAV / CardDAV | `http://127.0.0.1:1080/caldav/` and `/carddav/` |
| JMAP | `http://127.0.0.1:18080/jmap/session`; discovery at `/.well-known/jmap` |

Login:

- First connection for a new email opens a controlled Chromium window ([go-rod](https://github.com/go-rod/rod)).
- Sign in at Microsoft in that window. MFA and conditional access work there.
- The bridge never sees or stores the Microsoft password.
- No account setup is needed before the first connection.
- Sessions renew silently where possible. If Microsoft asks again (about every three months), the next operation waits for that window.
- Multiple accounts: add each with its own email, same bridge password.
- Work and personal accounts (`outlook.com`, `hotmail.com`) sign in the same way. The bridge does not treat them differently.
- Accounts stay separate: login, browser profile, session, state directory.
- Tests cover two-account isolation (registry, IMAP, SMTP, CalDAV/CardDAV) against fakes. Two real Microsoft accounts are a manual check.

## Flags

- All flags also work as `OB_<FLAG>` environment variables.
- No config file.

```text
o365bridge serve --help
```

- `--interface` (default `127.0.0.1`)
- `--imap-port` (1143), `--smtp-port` (1025), `--dav-port` (1080), `--jmap-port` (18080)
- `--password` (required; prefer `OB_PASSWORD`)
- `--graph-client-id` (empty: Graph off, OWA only)
- `--state-dir`, `--browser-path`
- `--help` groups flags for display only. Names, defaults and env vars do not change with grouping.

## TLS (optional)

- Encrypts the existing ports. One PEM cert/key pair for all endpoints.
- Design: [downstream TLS design](<docs/superpowers/specs/2026-10-14-downstream-tls-design.md>)

```sh
OB_PASSWORD='choose-a-bridge-password' ./o365bridge serve \
  --tls-enabled --tls-cert cert.pem --tls-key key.pem
```

| Flag | Default | Notes |
|---|---|---|
| `--tls-enabled` | false | HTTPS for DAV/JMAP; TLS mode below for IMAP/SMTP |
| `--tls-cert` / `--tls-key` | unset | PEM cert (plus chain if any) / matching key |
| `--tls-min-version` | 1.3 | `1.2` or `1.3` |
| `--imap-tls-mode` / `--smtp-tls-mode` | starttls | `starttls` or `tls` (implicit) |

- Env vars: `OB_TLS_ENABLED`, `OB_TLS_CERT`, `OB_TLS_KEY`, `OB_TLS_MIN_VERSION`, `OB_IMAP_TLS_MODE`, `OB_SMTP_TLS_MODE`.
- Bad TLS options or files stop startup. No plaintext fallback.
- Cert is loaded once. Restart to replace it.
- URLs become `https://localhost:1080/caldav/`, `/carddav/`, and `https://localhost:18080/jmap/session` (discovery also at `https://localhost:18080/.well-known/jmap`).
- Port does not move when you pick a TLS mode:
  - STARTTLS: pick STARTTLS in Thunderbird, full email as username, Normal password. SMTP clients send EHLO again after the upgrade.
  - Implicit TLS: set the matching `--*-tls-mode tls`, pick SSL/TLS in Thunderbird, keep the same port.
- Client must trust the CA. Private CA: install its cert, or set Himalaya `tls.cert` (see [Himalaya example](<examples/himalaya/config.toml>)).
- Hostname or IP you connect to must be in the cert SANs (`localhost`, `127.0.0.1`, etc.).
- Do not turn off certificate verification.

## Calendars

Readback and identity:

- OWA finds IDs with `FindItem`, then loads full fields with batched `GetItem` before date filtering. Paging goes past 1,000 items.
- Incomplete readback is an error. Events are not quietly hidden.
- Event title is never an identity key. UID wins; cloud item ID is the fallback, in URL and iCalendar data.
- IDs with `/`, `+`, `%` round-trip as one URL segment, decoded once.
- Same-title events stay separate (tested through query, GET, absolute multiget).

If an event is missing locally:

- After a `PUT` that returned `201`: restart the current build, refresh the client calendar.
- Check Outlook on the web before recreating anything. Recreating can duplicate it.
- Debug lines to capture from one refresh: `owa calendar listing`, `dav calendar report` (counts only, no contents or raw IDs), run with `--log-level debug`.
- Known gap: different item IDs can share one nonempty UID and collide. Do not rewrite shared UIDs to item IDs; that can break recurrence and invitations.

Recurrence:

- Whole series only. Daily, weekly with weekday lists, and representable monthly/yearly rules (absolute or relative), with count, inclusive end date, or no end.
- Anchor is the local calendar date. Timed `UNTIL` must be UTC DATE-TIME; all-day `UNTIL` must be DATE.
- `TZID` wall times are kept across DST. Clients need IANA tz data. Custom `VTIMEZONE` is not supported.
- Full CalDAV PUT can change or clear a supported rule. Ordinary edits keep native recurrence as is.
- Partial provider updates touch recurrence only with `Event.RecurrenceSet=true`.
- Unknown native rules are marked unsupported and kept. CalDAV will not replace or move those series.
- Invitation replies go separately from recurrence changes.
- Rejected, not dropped: `RDATE`, `EXDATE`, `RECURRENCE-ID`, multiple RRULEs, and rules the native API would clamp to another date.
- Monthly days 29 to 31 and February 29 yearly rules: use Outlook. See native [monthly](<https://learn.microsoft.com/en-us/exchange/client-developer/web-service-reference/absolutemonthlyrecurrence>) and [yearly](<https://learn.microsoft.com/en-us/exchange/client-developer/web-service-reference/absoluteyearlyrecurrence>) patterns.
- Series PUT returns no ETag (native normalization can change the data). GET again before the next conditional edit.
- Stale `412`: refresh and retry. Do not force the overwrite.
- Not done: occurrence/exception editing, shared-UID series identity, OWA expansion into later windows, full JMAP recurrence editing, saving the first CalDAV UID on new Graph events.
- Tested against fake HTTP servers. Live Outlook/client checks are manual.

Categories:

- Outlook category names sync to CalDAV `CATEGORIES` (OWA and Graph), including Unicode and commas/semicolons/backslashes.
- Sync before editing, or stale data can drop categories. Stale `If-Match` PUT gets `412`; a missing event is not recreated.
- Removing the last category in a full PUT clears it upstream. Updates that omit the field leave categories alone.
- Names only. No Outlook category catalog or colors; Thunderbird colors are local.
- Not covered: mail tags, contact categories, JMAP categories. Task categories work, see below.
- Design: [event categories design](docs/superpowers/specs/2026-10-15-event-categories-design.md)

## Tasks

Basics:

- Outlook tasks are task items, not appointments.
- CalDAV collections: Tasks holds VTODO, calendars hold VEVENT. Wrong type for a collection is rejected.
- Use the discovered Tasks collection in Thunderbird, not the calendar.
- OWA main task list: `http://127.0.0.1:1080/caldav/<email>/tasks/` (use HTTPS with TLS on). OWA custom task lists are not discovered.
- Graph finds To Do lists by opaque ID.
- Design: [task readback design](docs/superpowers/specs/2026-10-14-task-readback-design.md)

Readback and identity:

- OWA pages task IDs, then loads full fields in batches of at most 100. Partial readback is an error, not a short list.
- Client UID is stored on the native task (OWA string property, Graph open extension), so GET/query/multiget/update/delete find the same task after restart.
- No stored UID: cloud task ID is used in URL and iCalendar UID. Title is never a key.
- IDs with slashes, literal `%`, or `+` work in canonical URLs and in Thunderbird's re-encoded paths, decoded once.
- Missing multiget items get a per-resource error, not an empty success.

If Thunderbird tasks show in Outlook on the web but not locally:

- Restart the current build. Sync the Tasks collection.
- Old tasks may come back under cloud IDs. Their first Thunderbird UID was not stored and cannot be recovered.
- Do not recreate them. They exist upstream; recreating can duplicate them.
- Still missing: capture `owa task listing` and `dav calendar report` from one refresh at `--log-level debug` (counts only).

Details, dates, reminders:

- Categories and plain notes map both ways. HTML-only notes read as plain `DESCRIPTION`.
- Unchanged notes keep their native HTML. `ALTREP` data (e.g. `DESCRIPTION;ALTREP="data:text/html,..."`) is not read into the body. Same rule for events and contacts.
- Corrupted notes are not auto-fixed. Refresh, fix the text, save again.
- Reminders and native progress/state survive unrelated edits. The bridge does not guess replacements.
- Reminder support: one nonrepeating DISPLAY `VALARM`.
  - GET writes it as an absolute UTC `TRIGGER`.
  - Full VTODO PUT can set, change, or remove it. No alarm in the PUT means no reminder, so sync first.
  - Unchanged reminder is not rewritten, which keeps native fractional seconds.
  - Relative triggers need a zoned `DTSTART` or `DUE`. Days/weeks follow DST.
  - Rejected: DATE or floating anchors, unknown zones, DST gaps/folds, `DTSTART`+`DURATION`, repeated/multiple alarms, AUDIO/EMAIL alarms.
  - No custom alarm text, snooze, or dismissal state. Event alarms unchanged.
- DATE-only start/due keeps DATE form across restarts (native metadata). Bad non-empty dates are errors.
- No date is rewritten to UTC midnight/noon or a tenant zone.
- Graph: full update clears omitted dates/notes; partial update leaves omitted fields alone. Task and date-metadata writes are separate calls, so a failure can need a retry on the same task.
- Still limited: mixed DATE/date-time start and due, or equal start and due, on one native task.

Recurring tasks:

- Daily, weekly with weekdays, monthly by date or ordinal weekday, yearly by date. Count or end date.
- Bad or unrepresentable rules are rejected before any write.
- Unsupported native recurrence is kept on ordinary edits.
- Not converted to a different schedule: completion-based regeneration, odd weekday subsets, out-of-range timezones, exceptions.
- Month-end and leap dates, task timezone display: still need live checks.
- Date changes or completing an occurrence can split a task in OWA: one series plus one detached occurrence. Both stay in Outlook. The bridge does not delete or merge cloud rows.
- Series keeps the client UID. Detached copies get native-ID UIDs and no RRULE. A last remaining occurrence keeps the client UID.
- New splits get the detached copy its own bridge UID in a second, metadata-only write. If that write fails after the edit went through, sync before retrying.
- Old copied-UID groups are not migrated on read. If the series later ends, leftovers can fall back to the stored UID or come back ambiguous. Ambiguous groups are errors; nothing is picked by response order.
- After a recurring PUT, GET a fresh ETag before the next conditional edit.
- Stale `412` on any task PUT: refresh and retry. Full PUTs still need current data.

Files:

- Existing native attachments show as authenticated `ATTACH` links.
- Download (GET/HEAD) needs bridge login, checks account/list/task/file ownership, caps at 32 MiB per file.
- Nothing is cached, and no client-supplied URL is fetched.
- Not done: attachment upload or delete, embedded items, cloud reference links. New or changed attachment refs in a PUT are rejected; omitting them keeps the native files.

## JMAP

- Mail, calendars, contacts on the JMAP port. Same email and bridge password.
- Client config: [examples/himalaya/config.toml](examples/himalaya/config.toml)
- Server code is a nested Go module others can reuse: [jmap/README.md](jmap/README.md); design: [JMAP library design](docs/superpowers/specs/2026-10-14-jmap-library-design.md)
- Limits:
  - Uploaded blobs live in memory only, 32 MiB per account. Gone on restart.
  - `Email/changes` and push only cover what this bridge saw in its per-account journal. Older changes return `cannotCalculateChanges` and the client resyncs.
  - Calendar/contact changes fall back the same way.

## Graph (optional)

- Off by default. Needs a tenant-admin app registration.
- Turn on: `OB_GRAPH_CLIENT_ID=<client-id>`
- Accounts try Graph first. Non-transient auth failure falls back to OWA.
- Registration to ask your admin for:
  - Organizations audience, public client with PKCE
  - Redirect: `https://login.microsoftonline.com/common/oauth2/nativeclient`
  - Delegated scopes: `offline_access`, `User.Read`, `Mail.ReadWrite`, `Calendars.ReadWrite`, `Contacts.ReadWrite`, `Tasks.ReadWrite`

## Logs

- Default: human-readable, info level (listeners, logins, IMAP/SMTP/DAV operations, upstream failures).
- Passwords, tokens, auth and cookie headers: never logged. No personal content at info/debug.
- `--log-level debug`: protocol summaries (`owa calendar listing`, `dav calendar report`, task listing lines above).
- `--log-level trace` adds:
  - `imap command` lines with account, peer, tag/name, auth/TLS state, mailbox. No arguments, passwords, SASL data, or APPEND bodies.
  - OWA/Graph request and response payloads, tagged by account, channel, operation.
  - Browser login and OAuth token calls are not traced.
- Rejected DAV PUT/DELETE logs account, operation, HTTP status at warning. No path, body, headers, or validators. `401` uses the normal auth log instead.
- Trace payload limits:
  - Only complete JSON, capped at 64 KiB in, 48 KiB out after redaction.
  - Oversized, broken, or non-JSON bodies are skipped with a reason and byte count. No partial dumps.
  - Base64 file bytes inside JSON are removed too. Base64 is not redaction.
  - Known secrets and credential fields are redacted, but redaction is not anonymization.
- Warning: trace logs contain mail, calendar, task and contact content.
  - Keep them private. Capture one reproduction only.
  - Redact before sharing. Go back to info/debug and delete the capture after.

## State and reset

- On disk: session cache only (found accounts, tokens/browser profiles, IMAP ID maps).
- Location: `$XDG_STATE_HOME/o365bridge` by default, one owner-only directory per account.
- Reset one account: stop the bridge, delete `accounts/<email>/` there, reconnect. That account logs in fresh.

## Not supported

- EWS, EAS
- OneDrive/WebDAV files
- Headless or multi-user use
- Management UI
- Other OSes are secondary. Linux desktop is the target.

## Development

```sh
export GOMODCACHE=$PWD/.gocache/mod GOCACHE=$PWD/.gocache/build GOPATH=$PWD/.gocache/gopath GOSUMDB=off
go test ./...
go vet ./...
```

- The `.gocache` vars are only for sandboxes with a read-only Go cache. Elsewhere, plain `go test ./...` is fine.
- Tests use fake Graph/OWA servers. No Microsoft account needed.
- Real-tenant acceptance is a manual check by the account owner.
