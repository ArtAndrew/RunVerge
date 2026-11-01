# Review financial changes after a backup — development candidate

`reconciliation-plan` compares the original restored snapshot with the later
source snapshot already saved through [evidence ingestion](RECOVERY-EVIDENCE.md).
The command is undergoing runtime validation. It is not in alpha.6 and does not
apply financial changes or permit new work.

Use the same private selectors, archive directories, keys and staging directory
as evidence ingestion. Run on the trusted operator host:

```sh
platform recovery reconciliation-plan \
  --source-intent-file "$PRIVATE/source-stop.json" \
  --target-config "$PRIVATE/target.json"
```

The output contains organization and reservation identifiers. Keep it private;
it is not a report for sharing across tenants. No ordinary tenant API exposes
this host command.

For each organization and reservation, `before` describes the original snapshot
and `after` describes the later source snapshot. **Neither means that the target
budget has been changed.** A null side means the identity is absent from that
snapshot. Amounts are exact decimal strings in micro-USD. Null actual cost is
distinct from a confirmed zero; unknown reservations count even when their
reserved amount is zero.

The plan includes:

- The exact source, target, archive receipts, snapshots, capture link and target
  schema fingerprints, plus a reproducible `plan_sha256`.
- Old and later budgets, booked costs, reservations, financial holds and unknown
  counts. Booked source costs are not independent proof of a provider's bill.
- New ledger, evidence, quarantine, resolution and incident references. Ledger
  facts include signed deltas and the synthetic marker. They are not applied a
  second time or summed once for every ancestor task.
- Blockers identifying missing or rewritten immutable financial records,
  changed reservation identity, a history that cannot continue the old
  checkpoint, opaque legacy history and late-only identities.

The command checks the committed evidence bytes, rechecks original target data,
validates the later snapshot and recomputes financial comparisons. It checks
ledger projections independently of stored account totals. Temporary validation
is rolled back before a plan is returned. There are no provider calls, new
ledger entries, reserve releases or permission changes.

`external_window_coverage_unproven` remains a blocker: the snapshots do not prove
that every external request has finished or that no further invoice can arrive.
The result always retains `reconciliation_state: pending` and
`execution_held: true`. A successful comparison is not permission to clear either
a recovery hold or a financial hold.

This candidate refuses plans exceeding 10,000 organizations, reservations,
changed financial records or blockers, or 8 MiB of output. It does not truncate
the report and describe the remainder as reconciled.

No apply command is implemented yet. The subsequent apply must recompute the
plan, verify current authority and external coverage, and record separate
append-only resolutions. Editing this JSON or presenting its digest alone must
never authorize accounting changes or replay historical tasks.
