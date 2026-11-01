# Preserve late source records — development candidate

These host commands preserve a later source snapshot inside an already imported,
held recovery target. The bounded checks below passed; these commands are not
included in alpha.6. Use synthetic data. They do not reconcile charges, restore user access,
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

## Checked scope

[Recovery validation](https://github.com/ArtAndrew/RunVerge/actions/runs/41339253045)
passed at `5be5a55cdda3105e4cc83d1dd841df0583f7ccd2`: exact baseline checks,
wrong-pair refusal, immutable batch bounds, before/after COMMIT connection loss,
fresh CLI inspection, archives produced by the actual pinned SQL023 binary,
and a previously imported SQL023 target upgraded to SQL024.

[Installed Linux validation](https://github.com/ArtAndrew/RunVerge/actions/runs/41338446042)
passed on amd64 and arm64 at `a2a0ee2`: a real bound source capture was ingested
into a separately initialized target, inspected after lost output, and retained
after target restart. A late organization remained evidence rather than active
business data. Repeated ingestion was refused and disposable resources were
removed. No real model was called. The later recovery commit changes only two
independent test expectations for the new tables.

These checks establish evidence retention under an execution hold. They do not
establish final provider charges, full source-wide isolation, new-work clearance,
a native macOS capture path or a released full restore/upgrade procedure.
