# o365bridge

A personal, single-user Go daemon that bridges Outlook 365 accounts to
standard open protocols, for tenants where IMAP/SMTP are disabled and
Microsoft Graph is not consented.

It runs on the same Linux desktop as your mail client. Downstream it
speaks plain **IMAP** (1143), **SMTP** (1025) and **CalDAV/CardDAV**
(1080) on loopback. Upstream it speaks **OWA** (the Outlook web app's
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
| IMAP server / port     | `127.0.0.1` / `1143`, no TLS (v1)      |
| SMTP server / port     | `127.0.0.1` / `1025`, no TLS (v1)      |
| CalDAV / CardDAV       | `http://127.0.0.1:1080/caldav/` and `/carddav/` |
| JMAP                   | `http://127.0.0.1:8080/jmap/session` (discovery: `/.well-known/jmap`) |

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

Key flags: `--interface` (default `127.0.0.1`), `--imap-port` (1143),
`--smtp-port` (1025), `--dav-port` (1080), `--jmap-port` (8080, 0
disables), `--password` (required, prefer `OB_PASSWORD`),
`--graph-client-id` (empty = Graph disabled, OWA only), `--state-dir`,
`--browser-path`. There is no config file.

## JMAP

The bridge also serves JMAP (mail, calendars, contacts) on its own
port, with the same username (your email) and bridge password. It is
powered by a standalone server-side JMAP library that lives in this
repository as a nested Go module — see [jmap/README.md](jmap/README.md);
other projects can implement its backend contracts and reuse it.

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
error whenever an operation fails for an account. Passwords,
tokens, headers and message bodies are never logged. Protocol
detail is available with `--log-level debug`.

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
OneDrive/WebDAV files, JMAP, local TLS, headless/multi-user
operation, management UI. Linux desktop is the supported target;
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
