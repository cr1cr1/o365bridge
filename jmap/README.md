# jmap — a server-side JMAP library for Go

A standard-library-only Go library for **being** a JMAP server. It is
not a client. You implement a few backend contracts against your own
storage; the library handles the protocol: session discovery, method
dispatch, result references, creation-id references, blobs, state and
changes, push, and the data types of the JMAP specification family.

Developed inside the o365bridge repository (which is its first
consumer) as a nested module, so it can be depended on without the
bridge's dependency tree:

```go
require github.com/cr1cr1/o365bridge/jmap v0.0.0
```

The module path will change when the library graduates to its own
repository; until then the public API may still evolve between
releases.

## What's implemented

| Area | Spec | Package |
|---|---|---|
| Core: session, dispatch, `/get` `/set` `/query` `/changes` `/queryChanges`, blobs, EventSource, PushSubscription | RFC 8620 | `jmap`, `jmap/server`, `jmap/datatype` |
| Mail: Mailbox, Email, Thread, Identity, EmailSubmission, VacationResponse, SearchSnippet | RFC 8621 | `jmap/mail` |
| Message disposition notifications | RFC 9007 | `jmap/mail` (`MDN/send`) |
| S/MIME status properties | RFC 9219 | `jmap/mail` (backend-supplied fields) |
| JMAP over WebSocket | RFC 8887 | `jmap/ws` |
| Quotas | RFC 9425 | `jmap/quota` |
| Sharing (principals) | RFC 9670 | `jmap/sharing` |
| Calendars (JSCalendar events) | JMAP Calendars / RFC 8984 | `jmap/calendars` |
| Contacts (JSContact cards) | JMAP Contacts / RFC 9553 | `jmap/contacts` |

For the not-yet-RFC specifications, each package's doc comment pins
the exact specification it follows.

## Shape

Three layers:

- **Wire types** (root package): Session, Invocation, Request/Response,
  the RFC error vocabulary, and generic `/get`/`/set`/`/query`/`/changes`
  argument and response shapes.
- **The engine** (`jmap/server`): an `http.Handler` doing discovery,
  authentication hand-off, sequential method dispatch with
  result-reference and creation-id resolution, upload/download,
  EventSource push, and PushSubscription storage + delivery. Method
  handlers register by name and capability; the session advertises
  only what is registered.
- **Backend contracts** (`jmap/datatype` + per-spec packages): a
  `Store[T]` per data type (get/create/update/destroy + state), an
  optional `ChangeStore[T]` for `/changes`, and small optional
  interfaces for everything else (blobs, submissions, availability,
  snippets, import/copy/parse…). Implement only what you can supply
  honestly; capabilities are advertised accordingly. A backend that
  cannot answer change queries returns `ErrCannotCalculate` and the
  library emits the spec's `cannotCalculateChanges` error so clients
  resync.

`datatype.Memory[T]` is an exported in-memory backend with a change
journal — useful for tests, examples, and very small deployments.

## Minimal example

```go
eng := server.New(myAuthenticator, server.Options{Blobs: myBlobs})
mail.Register(eng, myMailBackend)        // Mailboxes, Emails, Threads
calendars.Register(eng, myCalBackend)    // optional
http.Handle("/jmap/", eng.Handler())
http.Handle("/jmap/ws/", ws.New(eng))    // optional, RFC 8887
```

`myAuthenticator` decides who a request is (Basic, Bearer, or a
principal your own middleware authenticated — the contract is
credential-form agnostic) and which accounts the session may see.

## Honesty rules the design follows

- No silently fabricated data: server-computed properties a backend
  does not provide are omitted, never invented.
- State strings are opaque and backend-owned; `/changes` either
  answers truthfully or fails with `cannotCalculateChanges`.
- Limits advertised in the session (`maxObjectsInGet/Set`, sizes)
  are enforced by the generic datatype machinery.
