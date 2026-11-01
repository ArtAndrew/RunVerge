# Preserve late source records — development candidate

These host commands preserve a later source snapshot inside an already imported,
held recovery target. They are under runtime validation and are not included in
alpha.6. Use synthetic data. They do not reconcile charges, restore user access,
resume historical tasks or allow new execution.

First select and import the original [archive](RECOVERY-ARCHIVES.md). The
[bound source capture](RECOVERY-CONTAINER-STOP.md#post-backup-source-evidence-development-candidate)
must have saved a signed link between that archive and the later snapshot. Keep
the original private source plan, its `.capture` and `.capture-state` files,
both archive directories, the keys and the target intent. Apply the private path
and host trust requirements in those guides. Do not edit or regenerate selectors
to make unrelated snapshots match.

```sh
platform recovery evidence-ingest \
  --source-intent-file "$PRIVATE/source-stop.json" \
  --target-config "$PRIVATE/target.json" --confirm-ingest-evidence

platform recovery evidence-inspect \
  --source-intent-file "$PRIVATE/source-stop.json" \
  --target-config "$PRIVATE/target.json"
```

Ingestion verifies the authenticated pair, exact target and original imported
business data. It validates the later snapshot before atomically saving its
complete plaintext snapshot bytes, signed capture link and a record comparison.
The original business rows stay unchanged. The stored bytes are protected by
the target database's access and storage controls; the transport archive's
encryption does not encrypt these database rows at rest.

The report counts added, changed and missing records. “Missing” means absent
from the later snapshot, not proven deletion or permission to discard the old
record. Unmapped records have no accepted organization scope. These host-only
records are not a cross-organization user query interface. Comparison hashes
describe database row values; the saved complete snapshot retains the original
wire representation.

Only one evidence batch is admitted per restored target. A duplicate is refused.
If the command loses its connection or cannot print its result, inspect the
exact target instead of resending ingestion. Inspection checks the stored link,
payload and record counts; it does not rerun the comparison or establish final
provider billing. `reconciliation_state` remains `pending` and
`execution_held` remains `true`.

The candidate recognizes current SQL024 archives and an explicit SQL023 archive
profile, including a held SQL023 import upgraded to SQL024. This is not general
schema conversion or alpha.6/SQL022 upgrade support. Full installed recovery,
financial reconciliation and permission to start new work remain unfinished.
