# o365bridge

A personal, single-user Go daemon that bridges Outlook 365 accounts to
standard open protocols, for tenants where IMAP/SMTP are disabled and
Microsoft Graph is not consented.

It runs on the same Linux desktop as your mail client. Downstream it
speaks **IMAP** (1143), **SMTP** (1025), **CalDAV/CardDAV** (1080), and
**JMAP** (18080) on loopback. Plaintext is the default; optional TLS
provides HTTPS and per-endpoint implicit TLS or STARTTLS. Upstream it
speaks **OWA** (the Outlook web app's
internal API, which cannot be disabled without breaking webmail) and,
when a tenant admin provides an app registration, **Microsoft Graph**.

Design: [docs/superpowers/specs/2026-10-14-o365bridge-design.md](docs/superpowers/specs/2026-10-14-o365bridge-design.md)

## Quick start

```sh
go build -o o365bridge .
OB_PASSWORD='choose-a-bridge-password' ./o365bridge serve
```

Then add an account in your mail client (Thunderbird, Himalaya, …)
exactly like a normal account:

| Setting                | Value                                  |
|------------------------|----------------------------------------|
| Username               | your Outlook 365 email address         |
| Password               | the bridge instance password (above)   |
| IMAP server / port     | `127.0.0.1` / `1143`, plaintext by default |
| SMTP server / port     | `127.0.0.1` / `1025`, plaintext by default |
| CalDAV / CardDAV       | `http://127.0.0.1:1080/caldav/` and `/carddav/` |
| JMAP                   | `http://127.0.0.1:18080/jmap/session` (discovery: `/.well-known/jmap`) |

The first time a client connects with an email the bridge has never
seen, the bridge opens its own controlled Chromium window (via
[go-rod](https://github.com/go-rod/rod)) where you sign in directly
with Microsoft — MFA and conditional access included. That login is
held until you finish it; your Microsoft password is never seen or
stored by the bridge, and no account needs to be configured in
advance. Sessions renew silently where possible; when Microsoft
forces re-authentication (roughly every three months), the next
client operation holds while the login window reappears.

Several accounts can be used at once: add each in your client the
same way, with the account's email as the username and the same
bridge password. Accounts are fully isolated from each other —
separate logins, browser profiles, sessions, and state directories —
so one account's re-login or failure never touches another. This is
covered by automated two-account isolation tests (registry, IMAP,
SMTP, CalDAV/CardDAV); trying it with two real Microsoft accounts
remains a manual check, since the tests run against fakes.

## Flags (all also available as `OB_<FLAG>` environment variables)

```text
o365bridge serve --help
```

Help separates flags into Logging, Profiling, Listeners, Authentication,
State and browser, and TLS groups. Grouping only changes help presentation;
flag names, defaults, and environment variables remain unchanged.

Key flags: `--interface` (default `127.0.0.1`), `--imap-port` (1143),
`--smtp-port` (1025), `--dav-port` (1080), `--jmap-port` (18080, 0
disables), `--password` (required, prefer `OB_PASSWORD`),
`--graph-client-id` (empty = Graph disabled, OWA only), `--state-dir`,
`--browser-path`. There is no config file.

## Optional TLS

Enable encryption on the **existing ports**, using one PEM certificate/key
pair for all downstream endpoints:

```sh
OB_PASSWORD='choose-a-bridge-password' ./o365bridge serve \
  --tls-enabled --tls-cert cert.pem --tls-key key.pem
```

| Flag | Default | Meaning |
|---|---|---|
| `--tls-enabled` | false | HTTPS for DAV/JMAP and the chosen IMAP/SMTP TLS modes |
| `--tls-cert` | unset | PEM certificate, optionally followed by its chain |
| `--tls-key` | unset | Matching PEM private key |
| `--tls-min-version` | 1.3 | Minimum version: `1.2` or `1.3` |
| `--imap-tls-mode` | starttls | `starttls` or implicit `tls` |
| `--smtp-tls-mode` | starttls | `starttls` or implicit `tls` |

The corresponding environment variables are `OB_TLS_ENABLED`, `OB_TLS_CERT`,
`OB_TLS_KEY`, `OB_TLS_MIN_VERSION`, `OB_IMAP_TLS_MODE`, and `OB_SMTP_TLS_MODE`.
Invalid TLS options or certificate/key files fail before any listener opens;
there is no fallback to plaintext. The certificate is loaded once: restart
when replacing it. Without `--tls-enabled`, the plaintext defaults above remain.

When enabled, use `https://localhost:1080/caldav/` or `/carddav/`, and
`https://localhost:18080/jmap/session`. JMAP discovery also works directly at
`https://localhost:18080/.well-known/jmap` and advertises HTTPS/WSS URLs.
IMAP/SMTP each select **one** mode; the port does not change automatically:

- **STARTTLS (default):** in Thunderbird select **STARTTLS**, full email username,
  and **Normal password**. Encryption is required before authentication. SMTP
  clients must issue EHLO again after upgrading.
- **Implicit TLS:** set `--imap-tls-mode tls` and/or `--smtp-tls-mode tls`, and
  select **SSL/TLS** in Thunderbird, using the same configured port.

Clients must trust the certificate's issuing CA. For a private CA, install its
certificate in the client's trust store or configure Himalaya's per-backend
`tls.cert` option; see the [Himalaya example](<examples/himalaya/config.toml>).
The hostname or IP used by the client must appear in the server certificate's
subject alternative names (e.g. `localhost` and/or `127.0.0.1`). Do not disable
certificate verification. The bridge remains a single-user service; TLS does
not change account selection, passwords, or upstream Microsoft connections.

Design: [optional downstream TLS](<docs/superpowers/specs/2026-10-14-downstream-tls-design.md>).

## Calendar readback

OWA calendar listing discovers item IDs with `FindItem`, then fetches complete
calendar fields with batched `GetItem` requests before applying date filters.
Discovery pages are followed beyond the first 1,000 items. Incomplete event
readback returns an upstream error rather than silently hiding events.

If a newly created event disappears after a successful CalDAV `PUT` (`201`),
restart the bridge with the current build and refresh the client's calendar.
The automated regression covers creation, GET, query/multiget, update, and
deletion through the real OWA adapter against a fake HTTP server; real-tenant
readback remains a manual check. Do not recreate events just because they are
missing from a client's view—check Outlook on the web first to avoid duplicates.

Event titles are display text, never identity keys. CalDAV preserves Outlook's
UID, falling back to the cloud item ID when it is missing, in both the resource
URL and iCalendar data. Opaque identifiers containing `/`, `+`, or `%` round-trip
without being split into URL segments or decoded twice. HTTP-backed regressions
verify separate same-title events through query, GET, and absolute multiget.

For a live calendar that still shows fewer events than expected, run with
`--log-level debug` and capture the `owa calendar listing` and
`dav calendar report` lines from one refresh. These show discovery, date-filter,
returned-item, unique-ID/UID, and duplicate-UID counts, without event contents or
raw item IDs. Distinct item IDs sharing a nonempty UID can still collide; recurring
series/exception handling is incomplete. Shared UIDs must not be blindly replaced
with item IDs, since that can break recurrence and invitation relationships.

## Recurring calendar events

Whole-series scheduled repetition is mapped to native OWA and Graph calendar
patterns: daily, weekly (including weekday lists/week start), and representable
absolute/relative monthly and yearly rules, with count, inclusive end date or
no end. OWA supplies required default intervals/selectors; recurrence edits and
clears now use the native field. Graph sends/reads its structured recurrence and
lists native single events/series masters through `/events`, not expanded
calendarView instances. Single-event overlap is filtered locally; masters are
conservatively retained for client expansion (not exact occurrence-window matching).
Creation requires a real native item ID; an unexpected success-shaped response
without one is not reported as a successful calendar save.

Rule anchors use the event's local calendar date. Timed `UNTIL` must be a UTC
DATE-TIME; all-day `UNTIL` must be DATE. An inclusive native end date never adds
an occurrence whose local start is after the requested cutoff. Repeating events
retain IANA `TZID` wall times instead of becoming fixed-UTC schedules across DST.
Windows timezone readback uses pinned CLDR global defaults, rather than choosing
an arbitrary alphabetical alias (which could select a no-DST zone). Existing
forward aliases remain approximate. Clients need IANA timezone data; custom
`VTIMEZONE` definitions and generated timezone components are not supported.

A complete CalDAV PUT can change or remove a supported rule; unrelated
GET/edit/PUTs preserve native repetition without rewriting it. Supported anchor
defaults, single ordinal weekday spellings and rule ordering compare equally.
Partial provider updates change repetition only with `Event.RecurrenceSet=true`.
Unknown/unrepresentable native rules carry an unsupported marker and survive
ordinary edits; replacing or moving those series through CalDAV is refused.
Invitation replies must be sent separately from recurrence changes, so an
invalid series edit cannot partially send a response.

**Limits:** event rules that native APIs clamp to another date are not silently
translated into skipping rules. Absolute monthly days 29–31 and yearly February
29/invalid fixed dates are conservatively unsupported; use Outlook for these
patterns. Explicit representable relative last-day patterns remain separate.
The underlying distinction is documented for native
[monthly](<https://learn.microsoft.com/en-us/exchange/client-developer/web-service-reference/absolutemonthlyrecurrence>)
and [yearly](<https://learn.microsoft.com/en-us/exchange/client-developer/web-service-reference/absoluteyearlyrecurrence>)
patterns.
OWA native interval bounds are enforced. `RDATE`, `EXDATE`, `RECURRENCE-ID`, and
multiple RRULEs are rejected rather than discarded. Occurrence/exception
editing, shared-UID series identity, OWA master expansion into later query
windows, and full JMAP recurrence editing remain incomplete. The limited JMAP
adapter rejects selectors/exceptions it cannot translate instead of dropping
them. Graph event resources still use cloud `iCalUId`: persistence of the initial
CalDAV client UID on a newly created Graph event is not implemented, so same-path
Graph create/GET identity parity is not claimed. The full create/GET/query/edit/
clear integration regression uses OWA, which writes the client UID natively.
Task recurrence codecs and mutation semantics are unchanged; shared canonical
timezone readback applies to all providers.

Series PUT responses omit ETags because native recurrence normalization can
change the representation; a fresh GET supplies the canonical ETag for the next
conditional edit. Restart the current build and refresh the appointment calendar
before editing; refresh/retry a stale `412` rather than forcing an overwrite.
Real HTTP adapter regressions cover native schema acceptance, readback,
unchanged-series preservation and explicit replace/clear. Actual Outlook cloud
visibility, client expansion and live tenant acceptance still need an owner-run
check; inspect Outlook before recreating missing items to avoid duplicates.

## Event categories

Outlook event category **names** are synchronized through OWA and Graph to
CalDAV `CATEGORIES`, including multiple categories, Unicode, and names containing
commas, semicolons, or backslashes. Category changes affect the event's ETag, so
clients can detect them on refresh. Restart the current bridge build and
synchronize the remote calendar **before editing** to fetch existing cloud
categories. Stale conditional VEVENT PUTs with `If-Match` are rejected with HTTP
412 before any update/invitation response; a missing event with `If-Match` is not
recreated. Refresh and retry rather than forcing an overwrite. This check is not
an atomic upstream compare-and-swap; clients sending unconditional full PUTs
still need fresh calendar data to avoid unintended category removal.

Normal GET-edit-PUT event updates retain the categories in the downloaded object.
Changing categories writes the native Outlook category array; removing the final
category in a full CalDAV PUT clears it upstream. Programmatic/JMAP provider
updates that do not supply this field leave cloud categories unchanged.

This maps names only, not Outlook's master-category catalog or color palette.
Thunderbird category colors are local client settings. Mail tags, contact
categories, and a JMAP category mapping remain outside scope; task category names
are supported separately as described below. HTTP-backed
regressions cover cloud readback through GET/query/multiget, category edits and
clearing, and same-title sibling isolation; live client/tenant acceptance remains
an owner-run check.

## Tasks (Thunderbird / CalDAV)

Outlook tasks are native task items, not calendar appointments. The bridge
exposes task lists as separate CalDAV collections supporting **VTODO**, while
appointment calendars support **VEVENT**. In Thunderbird, use the discovered
Tasks collection, not the appointment calendar, and synchronize that collection.
OWA exposes the primary task folder at the stable URL
`http://127.0.0.1:1080/caldav/<email>/tasks/`; custom OWA task-list discovery is
not implemented. Graph discovers To Do lists using their opaque IDs. Use HTTPS
when downstream TLS is enabled.

OWA discovers task IDs in pages and hydrates complete task fields in batches
of at most 100. Sparse discovery rows are not used as complete tasks, and
incomplete readback fails instead of returning a misleading partial list.
Client task UIDs are saved with the native task: a named string property for
OWA and an open extension for opt-in Graph. This allows GET, query/multiget,
update, and delete to resolve the same task after a bridge restart. Tasks
without bridge UID metadata use their unique cloud task ID in both the URL
and iCalendar UID; task titles are never identity keys. VTODO/VEVENT writes to
the wrong collection are rejected rather than converted.

If tasks created in Thunderbird appear in Outlook on the web but not locally,
restart the current bridge build and synchronize the remote Tasks collection.
Older creations did not store Thunderbird's UID; their original UID cannot be
recovered reliably and they are fetched under their cloud IDs. **Do not recreate
them blindly**, since they already exist upstream and retries can add duplicates.
If they are still missing, run with `--log-level debug` and capture the
`owa task listing` (OWA accounts) and `dav calendar report` lines from the same
refresh. The listing logs contain counts, not task titles, bodies, raw IDs, or
credentials.

HTTP-backed tests cover native task creation, full readback, handler/provider
reconstruction, query/multiget, update, completion/reopening, deletion, and
legacy cloud-ID fallback. These are not a live Thunderbird/Microsoft acceptance
check. Advanced recurring-task exceptions and complete reminder/progress
fidelity remain limited.

### Task details and files

Task category names and plain notes now map end to end. Native HTML-only notes
are presented as plain DESCRIPTION text; unchanged notes retain their original
rich formatting upstream. Existing reminders and native progress/state are
preserved on unrelated edits rather than reset to a guessed value. Reminder
editing through VTODO alarms is still unsupported.

Thunderbird DATE-only start/due values retain DATE precision through restart,
using native task metadata. Existing ISO date/time values remain supported;
invalid nonempty dates fail explicitly instead of disappearing. No guessed
UTC-midnight/noon or tenant-timezone rewrite is applied. Graph full task updates
clear omitted nullable dates/notes; partial provider updates leave unsupplied
fields alone. Graph task updates and DATE-metadata extension updates are separate,
non-atomic requests; failure is returned and retry uses the same task.

Common scheduled repetition maps through native task-specific schemas: daily,
selected weekdays/weekly intervals, month dates or supported ordinal weekdays,
yearly dates, and count/end-date ranges. Invalid or unrepresentable rules are
rejected before writing. Completion-relative regeneration, arbitrary relative
weekday subsets, incompatible timezone ranges, and recurrence exceptions are
not silently changed to a different schedule. Unsupported native repetition is
preserved on ordinary edits. Native clamp/skip behavior for month-end/leap dates
and actual task timezone display still need live acceptance.

Existing native **file attachments** appear as authenticated bridge ATTACH links.
GET/HEAD downloads use the bridge email/password, validate account/list/task/file
ownership, and are bounded to 32 MiB per file; content is never cached locally or
fetched from caller-supplied URLs. Files are served as downloads with safe MIME
and filename headers. A client opening the link may prompt separately for bridge
authentication. Embedded items, cloud reference/source links, and native file
**upload/delete synchronization are not implemented**. New/changed attachment
references are rejected rather than reported as uploaded; clients omitting
attachment properties preserve existing native files.

Restart the rebuilt bridge and **synchronize the remote Tasks collection before
editing cached tasks**. Stale conditional task PUTs return 412 before updates;
refresh and retry. Unconditional full PUTs still require fresh data. If a due
date/note is still absent, provide the sanitized native field value and emitted
VTODO field from the same refresh; basic support exists, so the live cause cannot
be inferred from the symptom alone.

## JMAP

The bridge also serves JMAP (mail, calendars, contacts) on its own
port, with the same username (your email) and bridge password. A
ready-made client config lives at
[examples/himalaya/config.toml](examples/himalaya/config.toml). The
surface is powered by a standalone server-side JMAP library that
lives in this repository as a nested Go module — see
[jmap/README.md](jmap/README.md); other projects can implement its
backend contracts and reuse it.

Two bridge-specific limits, stated plainly: uploaded blobs live in
memory only (gone on restart, 32 MiB per account), and change
tracking (`Email/changes`, push) is based on a small per-account
journal of message IDs and hashes recording what the bridge has
personally witnessed — asking about anything older returns the
spec's `cannotCalculateChanges` and the client resyncs. Calendar and
contact changes currently use that same fallback beyond the current
state.

## Graph (optional)

Graph is disabled by default because it needs an app registration
created by a tenant admin. If your admin provides one, pass its
client ID: `OB_GRAPH_CLIENT_ID=<client-id>`. Accounts then probe
Graph first; any non-transient auth failure falls back to OWA
automatically. Recommended registration: organizations audience,
public client with PKCE, redirect
`https://login.microsoftonline.com/common/oauth2/nativeclient`,
delegated `offline_access`, `User.Read`, `Mail.ReadWrite`,
`Calendars.ReadWrite`, `Contacts.ReadWrite`, `Tasks.ReadWrite`.

## Logs

The bridge logs with zerolog at human level by default: listener
startup, per-account authentication (success at info, failure as a
warning), operations such as IMAP append/copy/move, SMTP sends,
and CalDAV/CardDAV puts/deletes, plus a warning with the upstream
error whenever an operation fails for an account. Passwords, tokens and HTTP
authentication/cookie headers are excluded. Personal content is not logged at
info/debug; explicit trace enables upstream payload diagnostics (see below).
Protocol summaries are available with `--log-level debug`. Individual `imap command`
records are trace-only (`--log-level trace`): account (empty before login),
remote connection address, command tag/name, authentication/TLS state, and
selected mailbox context. Command arguments, passwords, SASL responses and
APPEND literals are not included. Authentication and failure records keep
their normal info/warning levels.

To diagnose native upstream requests, restart with `--log-level trace`
(and optionally `--log-format json`). OWA and Graph provider HTTP calls emit
separate request/response payload records with account, channel, operation and
request correlation. Browser sign-in and OAuth token exchanges are not traced.
Known session secrets and credential fields are redacted; HTTP auth/cookie
headers are never included. Only complete JSON is rendered: capture is bounded
to 64 KiB and sanitized JSON output to 48 KiB. Oversized, incomplete, malformed
and opaque non-JSON MIME/base64/binary bodies are omitted with a reason and
observed-byte count, never dumped as unsafe prefixes. Known native MIME/file
byte fields inside JSON are also omitted: base64 is reversible, not redaction.
Tracing does not change request bytes, response parsing, or existing attachment
size limits.

**Trace logs contain personal mail, calendar, task and contact content.** Keep
them private, capture only one reproduction, inspect/redact
before sharing, then return to info/debug and remove the capture when no longer
needed. Redaction is not an anonymization guarantee.

## State and resetting an account

The only thing on disk is an interim session cache (discovered
accounts, tokens/browser profiles, IMAP ID mappings) in the state
directory (`$XDG_STATE_HOME/o365bridge` by default), one
owner-only directory per account. To reset one account, stop the
bridge and delete `accounts/<email>/` inside it; the next client
connection logs that account in fresh. There is no local mail store:
the bridge translates requests live and keeps no copy of your mail.

## Not in v1

EWS and EAS, personal Microsoft accounts (outlook.com/hotmail.com),
OneDrive/WebDAV files, headless/multi-user operation, management UI.
JMAP and optional local TLS have since been added. Linux desktop is the supported target;
other OSes are secondary.

## Development

```sh
export GOMODCACHE=$PWD/.gocache/mod GOCACHE=$PWD/.gocache/build GOPATH=$PWD/.gocache/gopath GOSUMDB=off
go test ./...
go vet ./...
```

(The `.gocache` env dance is only needed in sandboxes with a
read-only Go cache; on a normal dev machine plain `go test ./...`
works.)

Tests run against fake Graph/OWA servers — no Microsoft account is
needed. Live acceptance against a real tenant is a manual gate for
the account owner.
