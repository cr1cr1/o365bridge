# o365bridge — Design

Date: 2026-10-14
Status: implemented (v1). All automated verification runs against
fake Graph/OWA servers; live-account acceptance is an owner-run
manual gate (see Verification) and has NOT yet been performed.
Rod browser login likewise has only a gated integration test
(`O365BRIDGE_ROD_IT=1`) — the capture flow is unproven against a
real tenant until the owner runs it.

## Intent and why

Some Outlook 365 tenants disable IMAP/SMTP, and do not grant Microsoft
Graph API access (no app registration / admin consent a normal user can
obtain). The account still works in Outlook on the web, but no
third-party mail client (e.g. Thunderbird) can use it: mail, calendar
and contacts are locked inside Microsoft's own surfaces.

o365bridge is a personal, single-user Go daemon that runs on the same
machine as the mail client. Downstream it speaks standard open
protocols so **any** standards-based client works. Upstream it speaks
whichever Microsoft channel the account actually permits. The
architecture follows a proven shape for this problem: several
upstream backends hidden behind a single internal operation set.
This bridge re-creates that architecture as a standalone protocol
translator.

Success bar: parity with what the account can do in Outlook on
the web today. This bridge must work against the account about
as well as the web client does — including the
Microsoft-imposed interactive re-login roughly every three months,
which is accepted behaviour, not a defect.

## Goals (first version)

- One running bridge serves **several** of the user's Outlook 365
  corporate (work/school) accounts, each isolated. Personal
  Microsoft accounts (outlook.com / hotmail.com / live.com) are
  **not** in v1 scope.
- Downstream, per account:
  - **IMAP** — receive, folder tree, read/star flags, move/copy,
    create/delete/rename folders, delete (via Deleted Items), move
    to/from Junk Email (spam / not-spam), drafts, search, IDLE.
  - **SMTP** — send mail.
  - **CalDAV** — the **full calendar feature surface**, not
    CRUD-only: view/edit calendar **events** including recurrence,
    reminders, attendees and participation/invitation responses
    (accept/decline etc. must round-trip upstream via the
    dedicated invitation-response operations), free/busy where
    the downstream protocol surfaces it, and **tasks** as
    `VTODO`s with the full field mapping (title, body,
    start/due/completed dates, priority, reminder, recurrence,
    status).
  - **CardDAV** — view and edit **contacts**.
- Upstream, per account: **Microsoft Graph** and **OWA** (the Outlook
  web app's internal `service.svc` JSON API), both covering mail,
  calendar/events+tasks, and contacts.
- Proving clients: **Thunderbird** (full surface) and **Himalaya**
  (Rust TUI; mail only, as an independent IMAP/SMTP implementation).
- "Filter" support means client-side filters work: the bridge is not
  involved except by providing prompt new-mail notification (IDLE)
  and rock-solid move/flag/search primitives.
- Mail "labelling" means filing into folders; mail category/tag mapping remains
  outside scope. Calendar event category names are mapped to CalDAV CATEGORIES
  under the later event-category request; master-category colors/catalogs and
  contact category mapping remain outside scope. Native task category names are
  also mapped under the subsequent task-detail request.

## Non-goals (first version)

- EWS (Microsoft disables it on Office 365 from 2026-10-01) and EAS —
  dropped entirely, not just deferred.
- OneDrive / files over WebDAV. JMAP (possible later addition).
- Server-side Outlook rules; mail/contact categories and Outlook master-category
  palettes; shared mailboxes and the corporate GAL are out unless they fall out
  naturally (builder: do not build them deliberately). Event and task category
  names are explicitly requested exceptions.
- Personal Microsoft accounts (see Goals). Windows and macOS are
  secondary: **Linux desktop is the only v1 gate**; other OSes must
  not block it.
- Local TLS (plain protocols on loopback for now; TLS is a named
  later addition), headless/server or multi-user operation, any
  config file, any management UI or web admin page.
- A local mail store or offline mode. The bridge is a thin, live
  translator (see below).

## Anti-goals (failure even if it "works")

- Seeing, asking for, or storing the user's Microsoft password
  anywhere in bridge code.
- Telemetry or any data leaving the machine.
- Re-login demands materially more often than the web client's (silent renewal
  must be exhausted first).
- Silently mangling data (recurrences, time zones, flags, task state)
  in translation. Lossy behaviour must be visible in logs at minimum.
- Storing tokens/passwords/headers or message bodies in logs.

## Decided constraints (from the project owner)

1. **Language/runtime:** Go. Existing scaffolding is `main.go` +
   `cmd/root.go` (Kong CLI, zerolog, godotenv); `ServeCmd` is
   referenced there but not yet defined — it is to be created.
2. **CLI only, no config file.** All runtime settings are Kong flags
   on `ServeCmd`, with Kong default values for ports/hosts and the
   automatic `OB_…` envars the existing parser already enables
   (`kong.DefaultEnvars("OB")` in `cmd/root.go`). `.env` loading
   already in `Run` stays.
3. **Same box, per user, Linux first.** Assumes a desktop session on
   the machine running the bridge (needed to show a login window).
   Linux desktop is the v1 build/test/release gate; Windows/macOS
   are secondary and must not drive v1 design decisions.
4. **Upstream login is browser-driven** via
   `github.com/go-rod/rod`: the bridge pops its own controlled
   Chromium window; the user signs in directly with Microsoft (MFA,
   conditional access included). No localhost-redirect OAuth flow in
   the system browser.
5. **Local client auth:** the mail client is configured like any
   normal account — email address as username, bridge host/ports for
   IMAP/SMTP/CalDAV/CardDAV. The client also supplies the **bridge
   instance password**: one password per bridge instance (not per
   account), set at startup (serve flag / `OB_…` env). The username
   (email) selects the account; the instance password authorises use
   of the bridge. Microsoft credentials never reach the mail client.
6. **No accounts at boot.** The bridge starts knowing zero accounts.
   Accounts are created dynamically: the first client connection
   presenting an unknown email triggers that account's login flow.
7. **Hold-and-wait.** A first login, and any later re-login, *holds*
   the client request that triggered it. The bridge first tries
   silent renewal (refresh token, live Rod browser profile); only if
   that fails does it pop the interactive login window — still
   holding the request. Failing the client request is the last
   resort (user closes the window, sign-in errors, …).
8. **Channel probing.** **Graph is disabled by default.** There is
   no built-in app registration: a tenant admin creates one and its
   client ID is supplied via serve flag / `OB_…` env (see below).
   Only when a client ID is configured is Graph probed, in the
   order **Graph → OWA**; otherwise accounts go straight to OWA.
   Any **non-transient** client/auth error on Graph — at probe time
   or later on a Graph-bound account — triggers automatic fallback
   to OWA with no manual state deletion. Transient failures
   (network, DNS, timeouts, throttling, 5xx) never cause fallback.
   Rationale: Graph is preferred where an admin has enabled it;
   OWA is the backstop because it cannot be disabled without
   breaking Outlook on the web itself. (Owner's account: Graph and
   IMAP disabled, OWA via web-login page — the motivating case.)
9. **Thin translator, minimal state.** Requests are translated
   on the fly; no local mail/calendar/contact store, no sync engine.
   The only persisted state is an *interim session cache*: discovered
   accounts, their chosen channel, tokens/session material (incl.
   the per-account Rod browser profile), and the minimal upstream-ID
   mapping IMAP needs to keep UIDs stable across connections.
10. **Logging (Go):** zerolog. Default (info) level logs only
    human-relevant events: listeners up (interface/ports), account
    discovered, channel selected, login window needed, silent renewal
    succeeded/failed, re-login completed, upstream errors that affect
    the user. Protocol-translation detail, request/response shapes,
    timings and probe attempts go to debug. No nonsense logging:
    no per-message info spam, no tokens/passwords/headers/bodies at
    any level (bodies not even at trace; token material never).
    The later contextual-logging request moves individual IMAP command records
    to trace and adds account, remote address, tag, TLS/authentication state and
    selected-mailbox context, without logging command arguments or literals.
    Existing authentication/failure info and warning levels are unchanged.

## Architecture

```
Thunderbird / Himalaya            o365bridge (per user, same box)            Microsoft
┌──────────────────┐   IMAP/SMTP   ┌───────────────────────────────────┐
│ any std. client  │ ────────────▶ │ listeners (plain, loopback)       │
│ user=email       │   CalDAV/     │ local auth: instance password     │
│ pass=instance pw │   CardDAV     │ dynamic account registry          │── Graph REST/JSON ──▶ graph.microsoft.com
└──────────────────┘               │ thin translators (per protocol)   │── OWA service.svc ──▶ outlook.cloud.microsoft
                                   │ provider interface (per account)  │        ▲              outlook.office.com
                                   │ session cache + Rod login manager │        │ login via
                                   └───────────────────────────────────┘   controlled Chromium (Rod)
```

Components (deep modules; exact Go package layout is the builder's):

- **`ServeCmd`** (in `cmd/`, completing the existing scaffolding):
  flags only (see Defaults below), starts listeners, the account
  registry, session cache and Rod login manager; graceful shutdown.
- **Listeners:** IMAP, SMTP submission, and one HTTP listener serving
  CalDAV and CardDAV under distinct paths. Plain (no TLS) in v1,
  bound to the configured interface.
- **Local auth:** username = account email (normalised), password =
  instance password. Unknown email → triggers account creation flow
  (hold-and-wait). Known email with dead session → silent renewal,
  then login popup (hold-and-wait).
- **Dynamic account registry:** in-memory map email → account, backed
  by the session cache so discovered accounts/tokens survive restart
  (this cache is *not* configuration; deleting it just forces fresh
  browser logins).
- **Provider interface:** one internal operation set covering
  mail, calendar and contacts: list/get folders,
  list message metadata, fetch full MIME, fetch attachment, create /
  send from MIME, move/copy/delete messages, set flags, search,
  list/sync calendar events and tasks, create/update/delete event
  and task, **respond to invitations / update participation**
  (the upstream dedicated invitation-response operations, not
  plain event writes), get free/busy,
  list/sync contacts, create/update/delete contact, new-mail
  notification. Each account binds one implementation:
- **Graph provider:** REST/JSON to `graph.microsoft.com`, delta-style
  sync where the protocol helps, immutable IDs. Auth: OAuth2
  auth-code flow executed inside the Rod window (authorize page,
  intercept the redirect/code, exchange + refresh at the token
  endpoint) using the admin-supplied client ID from the serve
  flags (absent client ID = this provider is never attempted).
- **OWA provider:** JSON calls to `<host>/owa/service.svc`, requests
  shaped like Exchange objects (`__type: "…:#Exchange"`,
  `RequestServerVersion: Exchange2013`).
  Hosts: `outlook.cloud.microsoft`, `outlook.office.com`.
  Auth: **no app registration** — Rod
  opens the real Outlook web login; the bridge captures the web
  app's own `Authorization` header (and/or the OWA canary cookie)
  from the controlled browser's traffic and replays it (the same
  capture points the web client itself exposes: token responses
  and `Authorization` request headers). The
  per-account persistent Rod profile keeps the Microsoft session so
  renewals are usually silent.
- **Rod login manager:** launches Chromium on demand only (login or
  interactive renewal), one profile directory per account, shared by
  the Graph and OWA flows for that account (so Microsoft SSO state
  carries between them), exits after credentials are captured.
  Never asks for or stores the Microsoft password in bridge code
  (whatever the browser profile itself holds is the browser's own
  session state).
- **Thin translators:** map IMAP/SMTP/CalDAV/CardDAV operations onto
  provider calls per request. IMAP UIDs are integers derived from
  the minimal persisted ID mapping (upstream ID ↔ folder-local UID);
  UIDVALIDITY must survive restarts. SMTP send goes through the
  provider's send operation; whether Sent-folder copies are made by
  upstream automatically or need an explicit create is per-provider
  behaviour the builder must verify and make consistent. Client-side
  Junk/not-junk is a move to/from Junk Email (plus any upstream
  "mark as junk" signal only if the provider exposes one cheaply —
  builder's call, must not block the move). IDLE: use upstream
  change notification where available (OWA notifications, Graph
  delta polling); the mechanism and poll interval are the builder's,
  but IDLE must wake promptly enough that client filters feel live.
- **Session/token cache:** per-user state directory (owner's call:
  the OS user state dir, e.g. `$XDG_STATE_HOME/o365bridge` or the Go
  `os.UserCacheDir` equivalent — builder picks one, permissions
  `0700`/`0600`). Contents: per-account channel choice, refresh
  tokens / captured OWA session material, Rod profiles, IMAP ID
  mapping. Tokens encrypted at rest is *not* required in v1
  (loopback personal tool, owner-only directory) — builder: do not
  invent a keyring integration unless it is trivial.

### Serve flags (Kong defaults are the interface contract)

Owner's calls marked ◆; the rest are settled here as designer's calls
(the owner can veto any of them):

- ◆ `--interface` (host to bind), default `127.0.0.1`.
- ◆ `--imap-port` default `1143`; ◆ `--smtp-port` default `1025`;
  ◆ `--dav-port` (CalDAV+CardDAV HTTP) default `1080`.
- ◆ `--password` — bridge instance password, no default; `serve`
  fails fast with a human-readable error if unset (via flag or
  `OB_PASSWORD`). Never logged.
- ◆ `--graph-client-id` default **empty**. Empty means Graph is
  disabled and every account uses OWA directly. A non-empty value
  (flag or `OB_GRAPH_CLIENT_ID`) opts in to probing Graph first,
  using that admin-created app registration. There is no built-in
  client ID and no separate graph-enabled flag in v1; do not reuse
  any third party's client ID.
- `--state-dir` default: per-user state dir (above).
- `--browser-path` default empty = Rod auto-discovers/downloads
  Chromium. (Designer's call; Rod's launcher behaviour.)
- Logging flags already exist in `RootFlags` (`--log-level`,
  `--log-format`) and are reused, not duplicated.

### Graph app registration (external, admin-owned)

There is **no built-in registration and creating one is not a
code task**. A tenant admin who wants Graph creates the app
registration and the resulting client ID is handed to the bridge
via `--graph-client-id` / `OB_GRAPH_CLIENT_ID`; without it Graph
stays disabled. Recommended registration configuration (guidance
for that admin, derived from the reference Graph integration):

- Sign-in audience: **organizations** (work/school accounts;
  personal Microsoft accounts are out of v1 scope).
- Public client (desktop, PKCE, no secret). Redirect URI:
  `https://login.microsoftonline.com/common/oauth2/nativeclient`
  (a Microsoft-hosted done page; Rod intercepts the navigation
  to it,
  no listener is opened).
- Delegated API permissions: `offline_access`, `User.Read`,
  `Mail.ReadWrite`, `Calendars.ReadWrite`, `Contacts.ReadWrite`,
  `Tasks.ReadWrite`. Tokens are requested as
  `https://graph.microsoft.com/.default` so consent covers the
  set; per-domain capability checks use the granted permission
  names (e.g. `Tasks.ReadWrite`).

### Probe state machine (owner-confirmed)

- If no `--graph-client-id` is configured, there is no probe:
  accounts bind to OWA immediately.
- If one is configured, the Graph probe *is* a full Rod sign-in +
  token exchange, followed by a cheap proof call
  (`GET /v1.0/me/mailFolders/inbox`). There is no cheaper honest
  pre-check.
- The probe is **all-or-nothing on mail only**: token + inbox +
  `Mail.ReadWrite` commits the account to Graph. Other domains
  degrade individually, never silently — if e.g. `Tasks.ReadWrite`
  was not granted, tasks are simply absent for that account and
  the fact is logged at info level, naming the missing
  permission.
- **Error classification decides fallback.** The builder must keep
  one explicit classification in the auth layer: *transient*
  errors (network, DNS, timeouts, 429, 5xx) are retried with
  backoff and never change the bound channel; *non-transient*
  client/auth errors (consent denied/revoked, invalid client,
  invalid scope/grant, tenant-disabled API, persistent 401/403 on
  a fresh token) trigger **automatic fallback to OWA** — at probe
  time and equally later on a Graph-bound account whose Graph
  access dies. Fallback updates the cached channel to OWA, needs
  no manual state deletion, and is logged at info level. (A later
  fresh start after the account's state directory is hand-deleted
  may probe Graph again if a client ID is still configured.)

### OWA host selection (owner-confirmed)

Personal Microsoft accounts are **out of v1 scope**, so no
`outlook.live.com` variant is built. For corporate accounts Rod
starts the sign-in at `outlook.cloud.microsoft` and the bridge
adopts whichever of the corporate OWA hosts
(`outlook.cloud.microsoft` / `outlook.office.com`) the completed
login actually lands on. A password-based Autodiscover fallback
must **not** be used — the bridge never sees the password.

### Hold-and-wait mechanics (designer's call)

Login/renewal is **single-flight per account**: concurrent IMAP,
SMTP, CalDAV and CardDAV connections for the same email all wait
on the one pending login; exactly one Rod window exists per
account at a time. Holds are **indefinite**: there is no
bridge-side timeout at all — a waiter is released only when the
login succeeds, upstream returns an actual error, or the user
cancels/closes the login in the bridge. (A client connection
going away naturally drops that one waiter; it does not cancel
the login the others are waiting on.)

### Account removal / reset (designer's call)

No CLI or UI for it in v1. The session cache layout must keep one
self-contained directory per account (profile, tokens, ID map,
channel) under the state dir, so that **deleting that account's
directory while the bridge is stopped** is the supported reset:
it forces fresh probing and a fresh browser login for that
account only. This layout property is a design requirement, not
an accident.

## Risks and spikes (resolve early in the plan)

1. **Rod capture of the OWA session.** The reference capture
   relies on in-browser webRequest APIs; the Go equivalent is Rod/CDP network
   interception of the token response / request headers on
   `outlook.cloud.microsoft` / `outlook.office.com`, plus cookie
   extraction from the profile. Spike this first against the owner's
   real account: log in, capture `Authorization`, replay a
   `service.svc` `FindFolder` call.
2. **Graph OAuth inside Rod with an admin-supplied registration.**
   Cannot be resolved by the developer alone: there is no
   developer-accessible live Microsoft account (see Verification).
   Build it against fakes; live consent/refresh behaviour is an
   owner-run manual gate that only becomes runnable when the
   owner supplies a tenant admin's client ID and a Graph-enabled
   account. Until then, no live-Graph claim may be made.
3. **OWA breadth vs promise:** confirm against the captured
   reference behaviour which calendar/task/contact operations
   `service.svc` supports (esp. tasks) and mirror them; anything
   OWA cannot do must be found here, not during downstream
   implementation.
4. **Graph tasks source:** Graph calendar holds events; tasks live
   in To Do lists. Decide the CalDAV mapping (task lists ↔ CalDAV
   collections supporting `VTODO`) per provider and keep it
   consistent across providers.
5. **IMAP semantics on a thin translator:** UID stability with only
   an ID-map cache, `APPEND` (drafts), `MOVE`, server-side `SEARCH`
   limits per provider (push down what each API supports, fetch
   metadata for the rest), and SMTP->Sent consistency. Prototype
   against both Thunderbird and Himalaya early.

## Verification (what "working" must demonstrably mean)

There is exactly **one live account — the owner's — and the owner
tests manually and gives feedback**. Developers have no live
Microsoft account: all automated work runs against fake Graph/OWA
servers (recorded reference shapes), and every live check below
is an owner-run acceptance gate with a feedback loop, not a
developer-run test. Graph live verification additionally requires
the owner to supply an admin-created client ID and a Graph-enabled
account; without those, Graph is verified against fakes only.
Platform gate: **Linux desktop** (Thunderbird + Himalaya);
Windows/macOS runs are secondary and never block v1.

Against the owner's real O365 account (OWA channel):

- Add account in Thunderbird with only email + bridge address/ports +
  instance password; held login completes via one Rod sign-in;
  folders, mail, calendars, tasks, contacts appear.
- Read/reply/send (Himalaya too); sent mail in Sent; move/label-by-
  folder, star, delete, junk/not-junk round-trip visibly in Outlook
  on the web; client filter filing works on IDLE-delivered mail.
- Create/edit/delete an event, a task (mark done), and a contact in
  Thunderbird; changes appear upstream, and upstream edits appear in
  the client.
- Kill upstream session (revoke/expire), fetch mail: silent renewal
  or one held popup, no client-visible password error.
- Second account added the same way while the first keeps working.
- Restart bridge: accounts reconnected from session cache without a
  browser window (unless Microsoft forces re-login).

- Calendar behaviour spot-checks: accept and
  decline a meeting invitation in Thunderbird and confirm the
  organizer-visible response upstream; edit one occurrence vs. a
  whole recurring series; mark a recurring task done.

Automated tests cover the translators against fake Graph/OWA
servers (recorded reference shapes), so CI needs no Microsoft
account; live-account checks stay owner-run manual gates.

## Implementation status and known deviations (2026-10-14)

The initial plaintext-only TLS deferral above is superseded by the
approved [optional downstream TLS design](<2026-10-14-downstream-tls-design.md>):
`serve --tls-enabled` uses a supplied PEM certificate/key pair, defaults to a
TLS 1.3 minimum, serves HTTPS for DAV/JMAP, and selects implicit TLS or
STARTTLS independently for IMAP/SMTP on the existing ports. STARTTLS requires
encryption before authentication; TLS-disabled behavior remains plaintext.
This does not change upstream transports or the single-user boundary.

Package map: `internal/bridge` (domain types, provider contract,
error classification, registry, session store, ID map),
`internal/graph`, `internal/owa`, `internal/rodlogin` (Rod login
manager), `internal/imapserver`, `internal/smtpserver`,
`internal/davserver` (hand-rolled stdlib protocol servers — a
deviation from the "prefer emersion/go-*" guidance, chosen for a
deterministic, dependency-light RFC subset), `cmd` (`ServeCmd`).

Time zones are handled centrally: IANA↔Windows zone ID conversion
lives in `internal/bridge` and is applied by both providers (OWA
rejects IANA IDs outright; Graph returns Windows names for
Outlook-created events). OWA event times are sent as wall-clock
times in the event zone with a `TimeZoneContext` header. Per-account
operation logging (auth, append/copy/move, sends, DAV mutations,
upstream failures as warnings) is implemented in the downstream
servers.

OWA calendar readback uses paged `FindItem` ID discovery without the mail-list
shape, followed by bounded `GetItem` batches requesting calendar fields before
date filtering. Sparse discovery rows must not be treated as complete events;
missing items or timestamps fail the read instead of becoming apparent deletions.
An HTTP-backed OWA/CalDAV regression reproduces the original `PUT 201` then
`GET 404` failure and verifies creation, query/multiget, update, and deletion
after hydration. Tests also cover multiple response messages, incomplete
readback, timezone/UID/attendee/recurrence fields, and discovery past 1,000 items.
This does not change cancellation semantics, DAV resource-name identity, or
recurring-master window expansion; real-tenant readback remains owner-run.

Calendar identity is independent of event titles. DAV keeps existing Outlook
UIDs and serializes its cloud-ID fallback when a provider UID is absent. Routing
splits escaped path segments before decoding; object filenames and absolute
multiget hrefs are decoded once so opaque IDs containing slashes or percent
sequences remain intact. Regressions cover independent same-title events with
distinct UIDs or ID fallbacks, GET/query/absolute multiget, and count-only debug
logs for OWA discovery/filtering and DAV output. This verifies conditional
identifier defects, not the cause of any particular live same-title report.
Nonempty shared UIDs and recurring-series/exception semantics are not rewritten.
The identity correction passed ten race-test repetitions for DAV, OWA, Graph,
and JMAP callers, plus vet and build. The full-suite attempt also hit an
intermittent `TestSMTPTLSCloseDropsConnections/implicit_handshake` shutdown
failure; the same failure reproduced from the unchanged `e362b03` baseline.
SMTP code was left untouched rather than expanding the calendar fix's scope.

Event category names now map from native OWA/Graph arrays through the shared
Event model into escaped iCalendar CATEGORIES. Full CalDAV PUTs retain supplied
names or remove omitted categories; category-unaware programmatic/JMAP updates
leave the cloud field unchanged. Conditional VEVENT PUTs reject stale If-Match
values before update/response and do not recreate missing events. Refresh old
client caches before editing; unconditional writes and external edits between
read/write are not protected by an atomic cloud precondition. Category palettes,
mail/contact categories, and a JMAP category schema remain outside scope. Task
category names are now supported under the task-detail follow-up.
See the [event-category design](<2026-10-15-event-categories-design.md>).

Native task readback is repaired separately from appointments. Task collections
advertise VTODO only. OWA discovers IDs without the mail-list shape and hydrates
complete task fields in batches of at most 100, with paged discovery and explicit
failure on incomplete responses. Client UIDs are persisted on native tasks:
OWA uses the named `PublicStrings` string property `O365BridgeTaskUID`; Graph
uses the `com.o365bridge.task` open extension in the initial task POST. Legacy
items without metadata use their cloud ID consistently in href and iCalendar
UID. No task title is used for matching and no task-content store is introduced.
Collection/component validation prevents VTODO writes into appointment calendars
or VEVENT writes into task collections. Nested alarms no longer discard task
properties following the alarm. HTTP-backed regressions exercise a fresh-handler
read after task creation, query/multiget, repeated PUT without duplication,
completion/reopening, and selective deletion. See the
[task readback design](<2026-10-14-task-readback-design.md>) for evidence and limits.
Live Thunderbird/tenant acceptance remains owner-run.

Task detail synchronization now includes native category names, plain-note
projection with rich-note preservation on unrelated edits, DATE-only metadata,
strict scheduled repetition and authenticated native file links/downloads. A
full Task PUT clears supported absent fields; partial provider edits preserve
unsupplied fields. Task If-Match checks precede mutation, and unchanged notes,
reminders, progress/state and unsupported repeat metadata remain untouched.
File metadata is listed without fetching bytes; account/list/task/file ownership
is revalidated for download, including registry recovery. Downloads are bounded
to 32 MiB, attachment-disposition, nosniff and private/no-store. Upload/delete,
embedded items/reference links and VTODO alarm editing are not implemented.
Graph task/date-extension updates are separate, non-atomic requests with errors
propagated and retry of the same task. No new task-content store or auth channel.
See the [task-detail design/evidence carrier](<../../aegis/work/2026-10-15-task-details/10-intent.md>)
for the requested field boundaries and remaining live checks.

Known fidelity limits, all explicit rather than silent:

- **Legacy task identity:** tasks created before client-UID metadata was saved
  cannot reliably recover their original Thunderbird UID. They remain available
  through cloud-ID fallback after refresh; no title-based migration is attempted.
  Task recurrence/exception and reminder/progress mapping is still a subset.

- **Calendar UID/recurrence identity:** distinct cloud items sharing a nonempty
  UID can produce the same DAV resource URL. Recurring masters are still filtered
  by their initial start/end; recurrence-ID and exception representation are
  incomplete. Live missing-event diagnosis must distinguish independent records
  from series occurrences before changing UIDs; debug counters reveal duplicate
  UIDs and date-filtered records without exposing event contents.

- **Event recurrence on write:** Graph event RRULE conversion remains absent,
  and OWA event conversion remains minimal. Task repetition now has separate
  strict OWA/Graph codecs for common native patterns/ranges; unsupported rules,
  completion-relative regeneration and ambiguous timezone cutoffs are preserved
  or explicitly rejected instead of silently changed. Native month-end/leap-day
  expansion and real tenant date display remain owner-run acceptance checks.
- **OWA large mail:** sends always use one `MimeContent` payload;
  `_o`'s >4MB attachment-splitting upload path is not implemented.
- **Graph append is non-atomic:** create-draft → move → flag PATCH;
  a mid-sequence failure can strand the draft in Drafts (error is
  returned, message is not lost).
- **Graph send:** raw-MIME `sendMail` cannot carry
  `saveToSentItems`; the server default applies.
- **Protocol subsets:** IMAP SEARCH text matching is over envelope
  metadata, not bodies; DAV codecs cover the field subsets listed
  in `internal/davserver`; Graph message size is reported as 0
  (Graph exposes no size field), so RFC822.SIZE can be 0 until a
  body is fetched.
- **Multiple accounts:** supported by construction (the client
  username selects the account; logins, sessions, browser profiles,
  ID maps, and state directories are all per account) and covered
  by automated two-account isolation tests in `internal/bridge`,
  `internal/imapserver`, `internal/smtpserver`, `internal/davserver`,
  and `internal/rodlogin`. Two *real* Microsoft accounts in parallel
  remain an owner-run live check.
- **Live behaviour unproven:** real-tenant Graph consent/refresh,
  OWA header capture via Rod, Thunderbird/Himalaya interop, and
  the ~3-month re-login rhythm are owner-run gates.

## Scaffolding note

`cmd/` does not compile as found: beyond the undefined `ServeCmd`,
`Deps`, `newDeps`, `warnInterruptedCLI` and `checkInterrupted` are
also undefined, and `appName` still says `optiscaler-manager`.
That scaffolding is copy-paste residue, **not** to be preserved:
an early builder task replaces it with the minimal `Deps` this
design needs (logger, flags, account registry, session cache —
explicitly no local store) and fixes the app name.

## Left to the builder

Go package layout; choice of IMAP/SMTP/WebDAV libraries (prefer
established ones, e.g. the emersion/go-* family, over hand-rolled
protocols); token-renewal timings and retry/backoff; upstream search
push-down details; poll intervals behind IDLE; exact state-dir
location; Chromium discovery/download mechanics; error taxonomy
surfaced to clients (which failures are IMAP `NO` vs dropped
connection, etc.).

## Provenance

Decisions were brainstormed with the project owner on 2026-10-14.
A builder-check review then produced eight answers from the owner
that supersede parts of the original brainstorm where they
conflict, and this document already reflects them: Graph is
opt-in via an admin-supplied client ID and **disabled by
default** (no built-in registration — this reverses an earlier
brainstorm decision); non-transient auth failures fall back to
OWA automatically; personal Microsoft accounts are out of scope
(reversing the original brief's "regardless of personal/
corporate"); calendar scope is full feature parity; holds
are indefinite; platforms are Linux-first; account reset is
hand-deletion of a clean per-account state directory; the
non-compiling `cmd/` scaffolding gets a minimal repair, not
preservation. Live testing is owner-run against his one account.
Upstream protocol evidence was gathered during design from
Microsoft's documented APIs and the observed behaviour of the
Outlook web client and reference Exchange integrations (including
the EWS→Graph migration rationale: "migrate to OWA, which needs
no consent"). EWS and EAS were dropped during brainstorming
(EWS: Microsoft retirement 2026-10-01; EAS: weakest channel, OWA
covers its cases).
