# Completion reflection

Task identity/readback fixes were insufficient to prove detail fidelity. Tests must exercise actual native request/response contracts through the downstream handler, not only fake providers that preserve every field in memory. This repair found independent failures in field presence, note projection, DATE metadata, recurrence shape/selectors/cutoffs and file transport.

The smallest safe attachment boundary is metadata plus authenticated, bounded native download, not arbitrary URL fetch or invented deep-insert/upload behavior. Explicit rejection is preferable to a successful response that silently drops a file or unsupported repeat change.

Projection fidelity is not native write authority: full VTODOs may normalize newlines, completion precision, date-time end bounds and rule ordering. Ordinary edits must compare the rendered semantics and retain rich notes, exact progress, reminders, native repeat anchors and unsupported patterns. Dedicated static-contract tests caught a relative-day enum/list mismatch; independent review caught cutoff/precision and partial-update loss.

No local task-content store, new authentication path or new dependency was introduced. OWA and Graph keep task-specific strict codecs because their native schemas and representability differ; event recurrence remains unchanged. Live tenant/timezone/occurrence and Thunderbird file-auth acceptance is still separate from the passing code/test gates.
