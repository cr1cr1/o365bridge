# Optional downstream TLS — approved design

## Intent and scope

Add optional encryption to o365bridge's existing `serve` command. HTTP
endpoints (DAV and JMAP, including JMAP WebSocket) use HTTPS/WSS when
enabled. IMAP and SMTP independently select either implicit TLS or
STARTTLS on their existing configured ports. One mode per endpoint;
no additional listeners and no protocol sniffing.

This document records the behavior approved by the owner. Implementation
proceeds directly in small test-first chunks with delegated IMAP and
SMTP work. There is deliberately no separate implementation plan.

## Command-line contract

| Flag | Default | Meaning |
|---|---|---|
| `--tls-enabled` | false | Enable downstream encryption |
| `--tls-cert` | unset | PEM certificate file (leaf followed by optional chain) |
| `--tls-key` | unset | PEM private-key file matching the certificate |
| `--tls-min-version` | 1.3 | Minimum accepted TLS version; supported values 1.2 or 1.3 |
| `--imap-tls-mode` | starttls | `starttls` or implicit `tls` when TLS is enabled |
| `--smtp-tls-mode` | starttls | `starttls` or implicit `tls` when TLS is enabled |

Kong exposes the corresponding `OB_TLS_ENABLED`, `OB_TLS_CERT`,
`OB_TLS_KEY`, `OB_TLS_MIN_VERSION`, `OB_IMAP_TLS_MODE`, and
`OB_SMTP_TLS_MODE` environment variables. Existing port defaults and
flags remain unchanged (IMAP 1143, SMTP 1025, DAV 1080, JMAP 18080;
JMAP port 0 disables its listener). There is no command rename.

Validate mode/version values before opening listeners. With TLS enabled,
load and validate the certificate/key pair before opening *any* listener;
missing files, malformed PEM, a mismatched key, or invalid options fail
startup. Never silently fall back to plaintext. With TLS disabled no
certificate is needed or read, and existing plaintext behavior remains.

TLS level means *minimum*, not an exact version lock. Use Go's TLS
implementation and default cipher policy. No TLS 1.0 or 1.1. Load the
certificate once at startup; certificate replacement requires restart.
The same configured certificate/key serves all enabled endpoints.

## HTTP behavior

When enabled, serve DAV and JMAP through `net/http`'s TLS serving path
with the validated TLS configuration. Do not offer a separate HTTP
listener or redirect plaintext clients to HTTPS. Preserve existing
routes, including direct JMAP discovery at `/.well-known/jmap`.

Session `apiUrl`, `uploadUrl`, `downloadUrl`, and `eventSourceUrl` must
be absolute HTTPS URLs on the connected endpoint. Both the JMAP session's
WebSocket URL (where currently supported) and the RFC 8887 WebSocket
capability's `url` must resolve to the matching WSS endpoint, with no
host duplication. Plaintext mode continues to advertise HTTP/WS.

Keep HTTP TLS-specific ALPN configuration separate from IMAP/SMTP;
standard HTTP/1.1 WebSocket upgrade must remain available. No new
WebSocket protocol features are in scope.

## IMAP behavior

Implicit TLS performs a bounded server handshake before the IMAP
greeting. STARTTLS mode greets in plaintext and advertises `STARTTLS`
and `LOGINDISABLED`, withholding authentication mechanisms until the
connection is encrypted. Reject `LOGIN` and `AUTHENTICATE` before
upgrade with tagged `NO [PRIVACYREQUIRED]` and do not inspect passwords,
start Microsoft login, or call upstream providers.

A valid STARTTLS command in the unauthenticated plaintext state flushes
a tagged OK before the TLS handshake, then replaces both protocol
buffers and re-evaluates capabilities. After upgrade, no `STARTTLS`
or `LOGINDISABLED` is advertised; normal authentication is available.
Reject repeated, authenticated-state, malformed, or unavailable
STARTTLS commands with standard protocol errors. No downgrade.

Never interpret bytes buffered before the TLS boundary as authenticated
commands. Reject a pipelined plaintext suffix (or close the connection)
rather than feeding it to the post-upgrade command parser. Bound
handshake duration; on failure log a warning and close, never resume
plaintext. Track and close handshaking/upgraded connections on shutdown.

## SMTP behavior

Implicit TLS handshakes before the SMTP greeting. STARTTLS mode
advertises `STARTTLS` on EHLO but not `AUTH` until encryption. AUTH before
TLS receives `530 5.7.0 Must issue STARTTLS first`, without initiating a
SASL challenge, decoding credentials, or accessing upstream services.
SMTP submission remains authenticated; plaintext MAIL before STARTTLS
is also rejected with the encryption-required 530 response.

STARTTLS succeeds only in the appropriate unauthenticated state, outside
an active mail transaction. Send and flush `220 2.0.0 Ready to start TLS`
before handshaking. Reset pre-TLS greeting/authentication and transaction
state; require a fresh EHLO before AUTH or submission. Subsequent EHLO
advertises AUTH and omits STARTTLS. Handle malformed/repeated/unavailable
STARTTLS requests with standard 501/503/454 errors as appropriate.

Apply the same safe buffer replacement, plaintext-pipeline rejection,
handshake deadline, failure-close behavior, and shutdown guarantees as
IMAP. Preserve TLS-disabled replies and existing AUTH mechanisms.

## Owning seams

- CLI owns validation, PEM loading, version selection, and HTTP listener
  setup; clones the validated configuration into each protocol server.
- IMAP and SMTP own their TLS/STARTTLS state transitions, capabilities,
  credentials gating, buffers, handshake deadlines, and connections.
- JMAP HTTP discovery owns URL scheme generation; changes there are
  confined to correct HTTP(S)/WS(S) transport advertisement.
- Existing Microsoft Graph/OWA authentication and transports are not
  modified. TLS does not change account selection or the bridge password.

Internal protocol server configuration may expose `TLSConfig *tls.Config`
and `StartTLS bool` without changing the existing constructor calls:
nil config preserves plaintext; non-nil config selects implicit TLS
unless StartTLS is true. Configuration is supplied before Serve begins.
No shared protocol state between accounts or connections.

## Acceptance and verification

Tests generate short-lived certificates in temporary directories; no
real accounts, production certificates, or private keys are committed.

- CLI defaults and environment overrides; accepted minimum versions;
  missing, malformed, or mismatched PEM; fail before any port is opened;
  plaintext configuration requires no certificate.
- HTTPS on DAV/JMAP with a trusted test CA; certificate verification
  fails for untrusted clients; TLS 1.2 is rejected by the 1.3 default
  and accepted only when the configured minimum permits it.
- Absolute HTTPS/WSS discovery URLs, direct well-known response, working
  TLS WebSocket upgrade; HTTP/WS advertisement regressions in plain mode.
- Both implicit TLS and STARTTLS on real sockets for IMAP/SMTP; no
  plaintext credential challenges or upstream activity before upgrade;
  post-upgrade authentication works with the same instance password.
- SMTP fresh-EHLO requirement and transaction reset; repeated and invalid
  upgrades; pre-TLS buffered bytes rejected; handshake failures/timeouts
  and shutdown with an active handshake cannot hang or downgrade.
- Existing plaintext protocol tests remain green. Run both Go modules'
  tests, race detector, vet, gofmt checks, and main binary build. Review
  connected security logic before committing implementation.
- Update README, client examples (Himalaya/Thunderbird instructions), and
  relevant original design notes. Clients must trust the certificate's
  issuing CA and connect using a matching DNS/IP subject alternative name;
  do not instruct users to disable certificate verification.

## Explicit non-goals

No additional ports/listeners, TLS autodetection, self-issued production
certificates, ACME, hot reload, client-certificate authentication, custom
cipher-suite flags, reverse-proxy trust configuration, or upstream API
changes. Do not extend other JMAP methods or repair unrelated conformance
issues while adding transport encryption.

## Implementation and verification record

Initial checkout: main at `9bfca01`; working tree was clean. The approved
design was committed as `5f757ef` before implementation. IMAP and SMTP
were delegated on disjoint files; the coordinator owns CLI/HTTPS and
integrated verification. No implementation plan was created.

Implemented surfaces:

- `cmd/tls.go` validates the approved flags, loads the PEM pair before
  account state/listeners, and selects Go's standard HTTP TLS path on a
  cloned configuration. Listener logs include security mode and minimum
  TLS version. Disabled TLS retains HTTP/plain IMAP/SMTP.
- IMAP/SMTP expose configuration-before-Serve TLS options, use bounded
  10-second default handshakes, and keep raw sockets tracked through
  upgrade so shutdown interrupts handshaking and active connections.
  Graceful handler exits close the current TLS wrapper and send
  `close_notify`; forced shutdown retains raw-socket interruption.
  STARTTLS privacy gates, capability changes, buffered plaintext
  rejection and SMTP's fresh-EHLO requirement are exercised on sockets.
- JMAP discovery corrects the TLS-specific duplicated WSS host and fills
  an unset WebSocket capability URL from the actual connection, preserving
  explicit endpoint configuration and other capability fields.

Actual test-first evidence includes initial failures for implicit
handshakes (plaintext greeting instead of TLS), pre-TLS authentication
challenges, upgrade/state handling, missing handshake deadlines, late
accept/shutdown handoff, missing CLI flags and HTTPS serving, and invalid
HTTPS/WSS discovery URLs. Each was followed by a passing increment.

Independent review reproduced one additional pre-TLS IMAP literal bypass:
`LOGıN` and `AUTHENTıCATE` (dotless i) could request a password literal
because the early gate used different case normalization from dispatch.
Both socket regressions failed before the minimal shared-normalization
fix and passed after it, including 100 repetitions. No extra parser
features were introduced.

A native OpenSSL check then exposed abrupt TLS termination: LOGOUT/QUIT
completed but the tracked raw socket closed without a TLS `close_notify`.
The reviewer confirmed the ownership issue in both handlers. New real-socket
record tests failed with a final TLS 1.2 application-data record (23), then
passed with a final Alert record (21) after one deferred close of each
handler's current wrapped connection. Raw shutdown tracking was retained.
Fresh TLS 1.3 OpenSSL checks subsequently exited cleanly in both modes.

Integrated evidence collected after the fixes:

- Main module: `go test -count=1 -race ./...`, `go vet ./...`, formatting
  and whitespace checks, and `go build -o bin/o365bridge .`.
- Nested JMAP module: `go test -count=1 -race ./...` and `go vet ./...`.
- New tests cover configuration defaults/environment variables, invalid
  PEM and mismatched keys, certificate trust and hostname rejection,
  minimum versions, HTTPS DAV/JMAP, WSS upgrade, all protocol privacy
  gates and transport transitions, and shutdown boundaries. Existing
  plaintext tests remain in the suites.
- The built executable was separately launched with temporary test PEM
  files and free ports in both modes. Verified HTTPS DAV/JMAP with a
  trusted certificate and TLS 1.3, IMAP pre/post-upgrade capabilities,
  SMTP encryption-required 530 and fresh-EHLO 503, and graceful shutdown.
  These tests never contacted Microsoft or opened browser login.
- Independent native OpenSSL clients verified certificate trust and DNS
  hostname, forced TLS 1.3, completed IMAP NOOP/LOGOUT and SMTP EHLO/QUIT,
  and exited 0 without unexpected EOF in all four combinations of mode
  and protocol, using the rebuilt executable after the lifecycle fixes.
- The Himalaya example parses as TOML and its port matches the unchanged
  JMAP command default. Vendor was refreshed from the nested module.

Remaining manual checks: actual client trust-store setup and certificate
selection on the owner's desktop. No production certificate, real
Microsoft account, or Himalaya/Thunderbird installation was used for the
transport tests; the independent interoperability client was OpenSSL.
Certificate renewal remains restart-only, as approved.
