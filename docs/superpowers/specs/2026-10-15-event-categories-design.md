# Event category synchronization

## Scope and cause

Repair Outlook event category names missing from Thunderbird/CalDAV. The clean baseline is `089fd8f`. The shared Event model has no category field, OWA does not request/map native Categories, Graph does not decode/map categories, and the iCalendar codec ignores CATEGORIES. This is missing translation, not a title/UID or recurrence defect.

TaskStartSnapshot: clean tree at `089fd8f`. Change Necessity: preserve user-visible native event metadata end to end. ArchitectureReviewRequired: narrow shared-field contract only, without a new store, API listener, or authentication path. The user's request supersedes the original non-goal for calendar event category names only; mail tags, task/contact categories, and Outlook master-category colors remain outside scope.

## Chosen behavior

- Event carries Categories as individual strings. Preserve spelling, ordering, Unicode, spaces, commas, semicolons, backslashes, and embedded newlines; no title matching or category catalog is introduced.
- OWA GetItem requests item:Categories. Native Categories arrays map to the event model. Graph event categories arrays do likewise. GET/query/multiget emit iCalendar CATEGORIES using per-value TEXT escaping and existing UTF-8-safe line folding.
- Parse repeated CATEGORIES properties and comma-separated lists, splitting only at unescaped commas before TEXT unescaping.
- Provider updates distinguish nil (not supplied; leave categories unchanged) from a non-nil empty list (clear). This preserves categories for existing programmatic/JMAP callers that do not map this field.
- A CalDAV PUT is a full event representation. Its category list is authoritative: a normal GET-edit-PUT retains categories, while omitting CATEGORIES or submitting an empty property removes them. This permits Thunderbird to remove the last category without requiring a nonstandard empty-property convention.
- Map supplied category names on native event creation/update. Clear them through verified native category mutation semantics. Leave invitation response selection unchanged.
- Category names affect event bytes and therefore ETags. Conditional VEVENT PUTs honor If-Match before either update or invitation response, accepting the current strong tag, a matching strong tag list (including repeated header lines), or an existing-resource wildcard. Stale/weak-only tags and If-Match on missing events return 412 without writes. Match rendering normalizes missing UIDs exactly as GET does. No task-precondition expansion.
- Refresh calendar data before editing after upgrade: pre-fix cached objects lack CATEGORIES. Conditional stale writes are rejected, but an unconditional full PUT still authoritatively removes omitted categories. This guard is not an atomic cloud compare-and-swap; intervening external Outlook writes between read and update remain possible. No logging of category names and no palette/color synchronization.

## Acceptance evidence

- Actual OWA adapter with fake HTTP serves a categorized cloud event through DAV GET, query, and absolute multiget.
- Native provider tests cover category read/create/update, explicit clearing, and nil-omission preservation for OWA and Graph.
- Codec tests cover escaped comma/semicolon/backslash/newline, Unicode, folded lines, repeated category properties, empty lists, and unrelated alarm properties.
- Editing only an event summary preserves categories after a complete readback; category edits/clearing reach the selected native event and not a same-title sibling.
- Neighboring task/calendar/JMAP tests, race checks, vet, and build remain valid. Live Microsoft/Thunderbird acceptance remains owner-run.

## Explicit boundaries

No change to category colors/catalogs, task categories, mail labels, contact categories, recurrence/exception identities, invitation response behavior, or JMAP calendar schema. Programmatic/JMAP updates that omit Categories must not clear the cloud's categories. Existing invitation response operations remain response-only; this repair does not introduce additional attendee-event updates.

## Verification

Implemented and independently reviewed. Category codec and provider regressions were observed failing before the mappings; stale conditional-write cases likewise failed before the event-only precondition check. The HTTP-backed OWA/DAV lifecycle verifies GET/query/absolute multiget, summary-edit preservation, category edits/clearing, changed ETags, and untouched same-title siblings. JMAP conversion retains nil Categories for category-unaware edits. Conditional-write tests cover pre-fix cached tags, fresh tags, weak tags, lists/repeated headers, wildcards, omitted headers, missing-UID normalization, deleted events, and rejection before an invitation response.

Verification: ten race-test repetitions for DAV/OWA/Graph/JMAP adapters pass; `go vet ./...`, bridge build, and `git diff --check` pass. The full `go test -race ./... -count=1` run passes every package except the previously established intermittent SMTP shutdown case `TestSMTPTLSCloseDropsConnections/implicit_handshake` (`Serve: use of closed network connection`). SMTP sources are unchanged; the same failure was already reproduced at the earlier unchanged baseline. This is not reported as a clean full-suite result.

No live Microsoft account or Thunderbird session was used. Restart the rebuilt bridge and refresh/synchronize calendars before editing to fetch category names and updated ETags. Category colors remain local client settings; unconditional full PUTs still require fresh data, and the bridge's If-Match check does not provide an atomic upstream compare-and-swap.
