# Reflection

The first bug belonged in the shared content-line parser, not in Outlook body cleanup: quoted ALTREP data URLs contained separators, and stripping body prefixes would have destroyed legitimate user text. A real request-shaped regression reproduced it before the small parser fix.

The second bug needed native semantics, not title-based deduplication. Microsoft legitimately splits task occurrences on date changes. Live read-only fields distinguished a continuing generator from copied recurrence metadata. Existing collisions need a bounded read projection, but fixing only projection would leave future identity unstable after the generator disappears. The write owner now assigns a separate bridge UID when the native response proves a new detached ID.

That added write exposed an authentication retry boundary: a metadata failure after native success must not replay the whole edit. A real HTTP401/Registry regression—not an unclassified service-code fake—proved the unsafe replay and the targeted guard.

The reminder follow-up belonged at the missing protocol seam, not in a new provider API: both backends already supported one reminder instant, but the codec discarded alarms and the handler forcibly restored the old value. Strict single-alarm validation and explicit timezone arithmetic prevent guessing unsupported semantics. Precision preservation required omitting unchanged native writes, not merely restoring a high-precision pointer before a millisecond serializer.

No cloud rows were deleted, no local content store was added, and no date precision was guessed to make tests green. Legacy ownership, strict date-representation and single-reminder limits remain explicit rather than disguised as full fidelity.
