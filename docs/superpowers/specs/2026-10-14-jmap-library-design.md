# JMAP server library and bridge integration — design

## Intent

o365bridge currently serves Outlook 365 accounts to local clients over
IMAP, SMTP, CalDAV, and CardDAV. JMAP is the modern, HTTP-native
equivalent protocol family, and some clients (Himalaya is the first one
that matters here, but the design must not be shaped by any single
client) speak it instead of IMAP/SMTP.

Rather than bolting a JMAP endpoint onto the bridge, this project will
grow a **standalone, server-side JMAP library** as a nested Go module
inside this repository. The library implements the JMAP standard
generically, behind narrow backend contracts (interfaces). o365bridge
is its first consumer ("dogfood"): the bridge implements the contracts
over its existing per-account Microsoft provider and serves JMAP on its
own port. Other projects must be able to import the library, implement
the contracts against their own storage, and get a conformant JMAP
server without inheriting anything about Microsoft, browsers, or this
bridge.

The library later graduates to its own repository. Everything below is
written so that move is a change of module path, not a rewrite.

## Goals

- A server-side Go library implementing the JMAP protocol family:
  - **Core** — RFC 8620: session object and discovery, method calls,
    result references, capability negotiation, the standard error
    vocabulary, blob upload/download/copy, `/get`, `/set`, `/changes`,
    `/query`, `/queryChanges`, state strings, EventSource push, and
    PushSubscription.
  - **Mail** — RFC 8621: Mailbox, Email, Thread, Identity,
    EmailSubmission, VacationResponse, SearchSnippet.
  - **MDN** — RFC 9007 (message disposition notifications).
  - **S/MIME** — RFC 9219 (S/MIME verification status properties).
  - **WebSocket** — RFC 8887 (JMAP over WebSocket, including push over
    the socket).
  - **Quotas** — the JMAP Quotas specification.
  - **Sharing** — the JMAP Sharing specification (principals and
    shareable objects), as far as the data types below expose sharing.
  - **Calendars** — the JMAP Calendars specification, on the
    JSCalendar data model (RFC 8984).
  - **Contacts** — the JMAP Contacts specification, on the JSContact
    data model (RFC 9553).
- Clear isolation: the library depends on nothing in this repository
  and nothing beyond the Go standard library; backends depend on the
  library; the bridge depends on both and connects them.
- "Full" includes the specs' optional methods, properties, and
  capabilities, not just the mandatory core. Where a feature depends
  on data only a backend can supply (S/MIME status is the obvious
  case), the library implements it fully and a backend that cannot
  supply the data simply does not advertise it.
- For specifications not yet published as RFCs (Quotas, Sharing,
  Calendars, Contacts at the time of writing), each phase pins the
  exact revision it implements — RFC number or named draft version —
  and records the pin in the library's own documentation before code
  is written against it.
- Backend contracts that make hard protocol obligations explicit —
  above all change history ("what changed since state X"), which the
  library cannot invent on a backend's behalf.
- o365bridge serving JMAP for real accounts: the bridge adapter maps
  the contracts onto its existing provider/registry machinery, with
  the same dynamic per-account login behaviour as the other protocols.
- Conformance evidence: the RFCs' example exchanges and method-level
  behaviours encoded as tests in the library; a real JMAP client
  against the bridge as the owner-run live gate.

## Non-goals

- **No client implementation.** The library is server-only; it does not
  talk to other JMAP servers.
- **No storage implementation.** The library ships contracts and
  protocol machinery, not a database. (An in-memory backend for tests
  is test infrastructure, not a product.)
- **No changes to the existing downstream protocols.** IMAP, SMTP,
  CalDAV, and CardDAV behaviour is untouched; JMAP is additive.
- **No local mail store in the bridge.** The existing rule stands. The
  bridge's JMAP support keeps only a small per-account journal of
  message IDs and content hashes (see below) — never bodies, never a
  sync engine.
- **No TLS termination** beyond what the bridge already defers; JMAP is
  served over plain HTTP on loopback, like DAV today.

## Anti-goals

- A bridge-specific library that only pretends to be reusable (types
  leaking `internal/bridge` concepts, Microsoft-shaped contracts, or
  behaviour only the bridge's provider can satisfy).
- Silent protocol approximations. Where a backend cannot honour a
  JMAP obligation, the library must surface the spec's defined error
  (e.g. `cannotCalculateChanges`) rather than inventing data.
- Client-specific workarounds. If a real client disagrees with the
  implementation, the RFC decides who is wrong.

## Decided constraints

These were settled with the project owner and are not open for the
builder to revisit:

1. **Full family, phased build.** Everything in Goals ships, in this
   order, each phase tested and committed before the next starts:
   1. Core engine, wire types, backend contracts, conformance harness.
   2. Mail (types and methods) — plus, in this phase, the bridge
      adapter, its change journal, and the bridge's JMAP listener, so
      the protocol is dogfooded as soon as Mail exists.
   3. Companions: MDN, S/MIME status, WebSocket, Quotas, Sharing.
   4. Calendars, then Contacts.
2. **Server-only library** (see Non-goals).
3. **Nested module for now.** The library lives in `jmap/` in this
   repository as its own Go module, so it builds and tests standalone
   (`cd jmap && go test ./...`) and other projects can depend on it
   without the bridge's dependency tree. Its module path will change
   when it graduates to its own repository; the bridge's imports are
   updated in the same commit when that happens.
4. **Standard library only** inside the library module. (Test-only use
   of the standard `testing` ecosystem included; no third-party
   runtime dependencies.)
5. **Contracts, not concretions.** All backend interaction goes
   through interfaces defined by the library. The library never
   imports the bridge; the bridge's adapter imports both.
6. **Bridge change journal.** The bridge backend keeps a small
   per-account journal (message IDs and hashes, timestamps, folder
   membership — no bodies, no headers beyond what hashing/comparison
   needs) in the account's existing state directory. Change queries
   and push are answered from what the bridge has personally
   witnessed; anything older than the journal yields the spec's
   `cannotCalculateChanges` error and the client resyncs.
7. **Own port for JMAP in the bridge.** JMAP does not ride the DAV
   port; it gets its own listener and flag, like the other protocols.

## Architecture

### The library (`jmap/` module)

Three layers, in dependency order:

- **Wire types.** Every object the RFCs define — session, accounts,
  capabilities, invocations, errors, and the per-spec data types
  (Mailbox, Email, Thread, Identity, EmailSubmission,
  VacationResponse, MDN, calendar events, contacts, quotas,
  principals, …) — with JSON behaviour matching the specs, including
  the awkward parts: patch objects, result references (`#` prefixed
  arguments), creation IDs, and unsigned/UTCDate handling. Types are
  plain data; they know nothing about HTTP or storage.

- **The engine.** A mountable `http.Handler` (plus the WebSocket
  upgrade path) that implements the protocol mechanics generically:
  - `GET /.well-known/jmap` discovery and the session resource,
    generated from the backend's account/capability description.
  - The API endpoint: request parsing, `using` capability checks,
    sequential method dispatch, result-reference resolution,
    `createdIds` bookkeeping, per-method and request-level errors in
    the spec's vocabulary, and the session's advertised limits
    (max concurrent requests, max size, max calls in one request…).
  - The upload and download endpoints, and `Blob/copy`.
  - `/changes` and `/queryChanges` bookkeeping on top of the backend's
    change contract, including `sinceState` validation and the
    `cannotCalculateChanges` fallback.
  - Push: the EventSource endpoint and PushSubscription management,
    fed by state-change notifications the backend emits through a
    contract; WebSocket transport per RFC 8887 carrying the same
    method dispatch and push. The engine also owns PushSubscription
    *delivery* as protocol mechanics: verification challenges,
    posting state changes to subscription callback URLs, expiry, and
    retry/give-up behaviour. Subscriptions themselves persist through
    a backend contract (below), since the library ships no storage.
  - A method registry so specs (and future extensions, and other
    projects' private capabilities) plug in as data-type packages
    registering their methods, rather than a hard-coded switch that
    grows forever.

- **Backend contracts.** Interfaces a project implements to supply the
  data and side effects. Guiding shape (names and exact splits are the
  builder's — the obligations are not):
  - An **authenticator / account source**: given the request's
    credentials (and any principal the host's own middleware already
    authenticated and attached to the request), produce the
    account(s) this session may act on, or reject. The contract is
    credential-form agnostic — HTTP Basic, Bearer tokens, and
    pre-authenticated hosts must all fit — because the library cannot
    know a future project's auth story. The library owns demanding
    credentials and emitting 401s; the backend owns deciding who
    someone is. Authorization is scoped by construction: a session
    only ever reaches the accounts its authenticator returned, and
    backends only ever return objects visible to that account, so
    per-object authorization (including shared objects) is enforced
    by what the backend's stores hand back, on every operation.
  - A **subscription store**: persistence for PushSubscriptions
    (create/fetch/delete, expiry), so the engine's delivery
    machinery survives restarts wherever the backend persists them.
  - Per-data-type **stores**: for each type, the operations behind
    `/get`, `/set`, `/query`, and `/changes` — fetch by IDs, apply
    creates/updates/destroys with per-record success/failure in the
    spec's shape, evaluate the spec's filter/sort vocabulary (or as
    much of it as the type defines), and report the type's current
    state string.
  - A **change source**: given a type and a prior state, list created,
    updated, and destroyed IDs since then, or report that it cannot
    (the library turns that into `cannotCalculateChanges`). Plus a
    state-change notification hook the engine's push machinery
    consumes.
  - A **blob source**: store/fetch/copy binary blobs by ID, backing
    upload/download and Email import/attachments.
  - An **identity/submission surface** for Mail: identities the
    account may send as, and "submit this email" as an operation
    (the rest of submission bookkeeping is ordinary `/set` on the
    EmailSubmission type).
  - Where a spec defines server-computed properties (thread
    membership, S/MIME status, quota usage, free/busy-style calendar
    computations), the contract states which side computes them; the
    library never silently fabricates a computed value a backend did
    not provide — the property is omitted or the call errors, per
    spec.

The library must be usable with a backend that implements only some
data types: capabilities in the session object advertise exactly what
the plugged-in backend supports, nothing more.

### Conformance harness (inside the library module)

- Golden tests from the RFCs' worked examples (request in, response
  out) for Core and Mail at minimum.
- Round-trip tests for every wire type: JSON in → object → JSON out,
  byte-stable for the fields the specs pin down.
- Method-level tests against an in-memory fake backend exercising:
  result references, creation IDs, partial failures in `/set`,
  `cannotCalculateChanges`, filter/sort evaluation, state mismatch
  handling, and push notification shape.
- A deliberately incomplete backend in tests, proving capabilities
  are advertised honestly and unsupported methods fail in spec shape.

### The bridge dogfood (main module)

- A new adapter package (e.g. `internal/jmapadapter`) implementing
  the library's contracts over `bridge.Registry` /
  `bridge.Provider`: the client username (email) selects the account
  exactly as in IMAP/SMTP/DAV; the bridge instance password
  authenticates; upstream access goes through `Registry.Call` so
  silent renewal and Graph→OWA fallback behave identically to the
  other protocols. The adapter is the only new bridge package that
  imports the library.
- The change journal lives in the adapter: per account, inside the
  existing `<state-dir>/accounts/<email>/` directory (owner-only
  permissions, same reset story — deleting the account directory
  deletes the journal). It records, per message the bridge has seen:
  upstream ID, folder, a content/flags hash, first/last seen times.
  It is a witness log, not a cache: entries exist to answer
  "created/updated/destroyed since state X" and to emit push state
  changes for witnessed differences. Population is JMAP-driven: the
  journal is fed when the adapter lists/fetches upstream data, and —
  while a push or EventSource subscriber is connected — by the
  adapter polling upstream listings for that account (the pattern
  IMAP IDLE already uses), journaling diffs and emitting state
  changes. The other protocol servers are not instrumented; their
  behaviour stays untouched.
- The journal generalises beyond email when later phases need it:
  calendar and contact change support (Phase 4) records the same
  ID-plus-hash witnesses for those types. Until a type has journal
  coverage, its state string is derived from a hash of the live
  upstream listing, and its `/changes` returns
  `cannotCalculateChanges` — honest, spec-legal, and explicitly the
  fallback everywhere in this design.
- Bridge-side uploaded blobs are ephemeral: held in memory by the
  adapter with a documented size cap and short lifetime, long enough
  for the import/submit workflows that consume them. Blob IDs do not
  survive a bridge restart; the library contract permits this
  (blob persistence is a backend property), and the bridge's docs
  must state it rather than let clients assume otherwise.
- A fourth listener in `cmd/serve.go` on `--jmap-port`, serving the
  library's handler. Session account ID for the bridge is the
  normalized account email (the builder may refine this inside the
  adapter; it is bridge policy, not library policy).
- Per-account operation logging in the established style: JMAP
  method calls and upstream failures logged per account (info for
  operations, warnings for auth/upstream failure), never credentials,
  tokens, or bodies.

## API stability

While the library is nested in this repository it is pre-1.0 in
spirit and in fact: contracts and wire APIs may evolve between
phases as later specs teach the design new shapes, and the bridge
absorbs those changes in the same commits. Graduation to its own
repository is the moment the public API freezes as v1; nothing in
this project may rely on external consumers before then, and the
graduation move is expected to be a module-path change plus that
freeze, not a redesign.

## Conformance oracle

There is no external JMAP conformance suite this project can run.
The oracle is the specifications themselves: their worked examples
as golden tests, their normative requirements as method-level tests,
and — for Mail — the owner-run live check of a real client against
the bridge. For Calendars, Contacts, Sharing, and Quotas, where no
client in hand exercises the surface, spec-derived tests are the
acceptance evidence, and ambiguities resolved during implementation
get recorded in the library's documentation next to the pinned spec
revision.

## What's left to the builder

- Package layout inside the `jmap/` module (the three-layer split
  above is the obligation; exact package names are not).
- Concrete contract interface shapes and method sets, within the
  obligations above.
- State-string encoding and journal file format, within "opaque to
  clients" and "IDs and hashes only".
- The bridge's `--jmap-port` default value and whether JMAP can be
  disabled with port 0 (consistent with the other flags' style).
- Registration mechanics for the method registry, provided extension
  packages never require editing the engine.
- Which spec-defined filter/sort/computed-property corners a given
  backend contract makes the backend's problem vs the library's —
  the line must be drawn per type, explicitly, in the contract docs.

## Acceptance shape

Per phase, acceptance tests first (the repository's ATDD rule):

- **Phase 1 (Core):** the library's conformance harness passes
  against the in-memory backend: session/discovery shape, method
  dispatch, result references, `/changes` happy path and
  `cannotCalculateChanges`, blob round-trip, EventSource state
  change received.
- **Phase 2 (Mail + bridge):** library Mail tests pass; bridge
  adapter tests (fake provider) prove per-account isolation for
  JMAP equivalent to the other protocols' two-account tests, journal
  answers `/changes` for witnessed changes and falls back honestly
  otherwise, and a scripted JMAP client session (session → Mailbox
  query → Email query/get → EmailSubmission set) succeeds end to end
  against the bridge binary. Owner-run live gate: a real JMAP client
  (Himalaya) against a real account.
- **Phase 3 (companions):** per-spec library tests; MDN send path,
  S/MIME properties surfaced when the backend provides them,
  WebSocket request/push round-trip, quota objects from backend
  figures, sharing types per spec. The bridge advertises a companion
  capability only where the Microsoft provider can supply its data
  honestly; companions with no upstream source ship library-only in
  this project until one exists.
- **Phase 4 (Calendars, Contacts):** library tests per spec; bridge
  adapter maps them onto the existing provider calendar/contact
  operations; the same data reachable over CalDAV/CardDAV and JMAP
  stays consistent because both read the same upstream live.

## Implementation status (as built)

All four phases are implemented and committed:

- **Library** (`jmap/` module, stdlib only): root wire types;
  `server` engine (session/discovery, dispatch with result-reference
  and creation-id resolution, blobs, EventSource, PushSubscription
  storage + async delivery, exported `Process`/`AuthenticateRequest`/
  `SubscribeStateChanges` for transports); `datatype` generic
  get/set/query/changes machinery with enforced object limits and
  `Memory[T]`; `mail` (RFC 8621 complete method coverage, MDN
  RFC 9007, S/MIME RFC 9219 properties); `ws` (RFC 8887, hand-rolled
  RFC 6455); `quota` (RFC 9425); `sharing` (RFC 9670 principals);
  `calendars` (JSCalendar); `contacts` (JSContact). Session objects
  advertise `webSocketUrl` when the host registers the websocket
  capability.
- **Bridge dogfood** (`internal/jmapadapter`, `--jmap-port` default
  8080): Basic auth into Registry hold-and-wait login; Mailbox/Email
  stores over the live provider (folder-encoded email IDs, MIME
  parsing, a metadata-only `Email/query` fast path), per-account
  `jmap-journal.json` answering `Email/changes` with version:offset
  paging and feeding push, ephemeral blob store, Identity,
  EmailSubmission (sends immediately; future `sendAt` is rejected),
  import/copy/parse/snippets; calendars and contacts mapped onto the
  provider with live-hash state and the `cannotCalculateChanges`
  fallback; WebSocket mounted at `/jmap/ws/`.
- A review pass after implementation fixed, among others: false
  "destroyed" reports after transient fetch failures, journal
  `maxChanges` paging losing records, moves trashing the original
  (now a real provider move), silent immutable-property drops in
  `Email/set`, and unbounded full-MIME `Email/query` scans.
- A conformance pass against the normative RFC texts then corrected
  the generated shapes: MDN/send is the RFC 9007 §2.1 batch method
  (`send: Id[MDN]` → `sent`/`notSent`, per-record error types
  including `mdnAlreadySent`, mandatory `onSuccessUpdateEmail`
  `$mdnsent` enforcement) with `MDN/parse` added; the S/MIME
  capability is `urn:ietf:params:jmap:smimeverify` with the RFC 9219
  §4 status vocabulary and `smimeStatusAtDelivery`; RFC 8887 push
  uses `WebSocketPushEnable`/`WebSocketPushDisable` honouring
  `dataTypes`, requests echo `id` as `requestId`, request-level
  errors are `RequestError` objects, and the websocket capability
  carries `url`/`supportsPush`. With no configured base URL the
  engine now absolutizes all session URLs from the request host, as
  RFC 8620 §2 requires.
- Still owner-run live gates: a real JMAP client (Himalaya) against
  a real Microsoft account; the library is otherwise covered by
  spec-derived tests in both modules.

## Known risks, stated plainly

- **`/changes` honesty.** The journal only knows what the bridge has
  witnessed. A mailbox changed from Outlook on the web between
  bridge sessions is discovered on next listing and journaled then;
  a client asking about a state older than the journal gets
  `cannotCalculateChanges` and resyncs. That is spec-legal and
  honest, but it means push and incremental sync are best-effort,
  not a guarantee — the docs must say so.
- **Spec size.** "The full family" is several thousand pages of
  normative text across the listed documents. Phasing exists
  precisely so each spec is implemented against its own tests, not
  approximated in bulk.
- **Computed properties.** Threading, snippets, and S/MIME status
  are only as good as what the upstream provider exposes; the bridge
  adapter surfaces what it can compute from fetched data and omits
  the rest, per contract.
