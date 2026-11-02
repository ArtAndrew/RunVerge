# Review financial changes after a backup — development candidate

`reconciliation-plan` compares the original restored snapshot with the later
source snapshot already saved through [evidence ingestion](RECOVERY-EVIDENCE.md).
The read-only command has passed the bounded checks described below. It is not
in alpha.6 and does not apply financial changes or permit new work.

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
plan, verify trusted operator authority and fact provenance, and record separate
append-only resolutions. Authentic financial facts must remain recordable while
external coverage is unresolved and execution stays held. Source fencing,
external coverage and fresh ordinary authority are separate prerequisites for
permitting new work. Editing this JSON or presenting its digest alone must never
authorize accounting changes or replay historical tasks.

## Checked development candidate

At `a3e4e49e8bba4180ba6952aa531d25e1a0f5e573`, the
[PostgreSQL checks](https://github.com/ArtAndrew/RunVerge/actions/runs/41342270302)
passed original/late comparison, refusal without a committed evidence batch,
rewritten immutable facts, financial history continuation, resource exhaustion,
saved SQL023 compatibility and fresh-process CLI checks.

The [installed Linux checks](https://github.com/ArtAndrew/RunVerge/actions/runs/41342276244)
passed on amd64 and arm64. A real captured archive was ingested into a separately
provisioned disposable target; its financial plan remained identical after a
database restart. Both receipts record `reconciliation_plan_restart_passed`,
retain execution hold and confirm fixture cleanup. No real model was called.
These checks do not establish financial apply, external coverage, new-work
permission, macOS recovery or full restore support.

## Financial recovery components in development

The development branch also contains a private financial executor and a verifier
that checks the original snapshot, each recorded change and the current database.
It changes financial records while keeping historical actions, task versions,
events and execution permissions unchanged. It does not expose an apply command.

The [full recovery check at e8a9935](https://github.com/ArtAndrew/RunVerge/actions/runs/41408623291)
passed settlement, negative correction, confirmed zero, duplicate/conflicting
invoice handling, overflow quarantine and repeated baseline verification.
The [accounting-only check at 5a7c4b3](https://github.com/ArtAndrew/RunVerge/actions/runs/41409692357)
also passed resolution of quarantined settlements and corrections after another
expense correction freed capacity. Insufficient capacity leaves the fact in
quarantine; repeating a completed resolution does not charge it again.

The [accounting-only check at ac6212a](https://github.com/ArtAndrew/RunVerge/actions/runs/41413153346)
cut the database connection before and after COMMIT. A fresh connection verified
the complete baseline and reported whether the exact operation was recorded,
without reapplying it. The storage inspector checks an operation ID and request
digest; it is not yet exposed as a host command. Verification uses temporary
tables and rolls back; it does not modify persistent business data.

These are synthetic financial facts in disposable PostgreSQL databases, with no
model or provider requests. The accounting-only runs are development checks,
not full release gates. A usable host apply path still needs authenticated fact
selection, ordering and source-to-target revision binding. The existing
`reconciliation-plan` command still requires the original target baseline; it
does not yet plan subsequent work on a financially reconciled target. None of
these components permits new execution or constitutes full restore support.
