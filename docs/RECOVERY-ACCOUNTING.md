# Record captured financial facts — development candidate

These trusted-host commands record a financial fact from a verified later source
archive into an already imported, held target. They do not restart tasks, contact
a provider or enable new work. They are not part of the alpha.6 download.

Complete [evidence ingestion](RECOVERY-EVIDENCE.md) first. Use the original and
later archives, their independently selected keys and the same private source
and target selectors. The operator needs access to those protected files and the
target database. This is not an ordinary Workspace or tenant API operation.

## Select and review a fact

Use [accounting-review](RECOVERY-ACCOUNTING-REVIEW.md) before or after recording
changes. Its `facts` identify source evidence, actual target identities and a
suggested next financial step. Keep the report private. The older
[reconciliation-plan](RECOVERY-RECONCILIATION.md) remains available to compare the
original and captured source snapshots before any target accounting changes.

Prepare a new operation file outside both archive directories:

```sh
platform recovery accounting-plan \
  --source-intent-file "$PRIVATE/source-stop.json" \
  --target-config "$PRIVATE/target.json" \
  --organization-id "$ORGANIZATION_ID" \
  --reservation-id "$RESERVATION_ID" \
  --source-evidence-id "$SOURCE_EVIDENCE_ID" \
  --out "$PRIVATE/invoice-plan.json"
```

Review the selected record, kind of change and `actual_microusd`. Amounts are
exact decimal strings in millionths of a US dollar. A known zero is distinct
from an unknown amount. The CLI accepts record identifiers, not edited invoice
amounts or reasons. Corrections require their preceding facts to be recorded
first. A reservation created only in the later snapshot cannot be introduced
into the target through this command.

The plan is saved before it is printed. If printing fails, the existing private
file may already be present; it will not be overwritten. A plan does not change
the target's financial records.

## Record it and inspect the outcome

```sh
platform recovery accounting-apply \
  --intent-file "$PRIVATE/invoice-plan.json" \
  --confirm-record-financial-fact

platform recovery accounting-inspect \
  --intent-file "$PRIVATE/invoice-plan.json"
```

Apply rechecks the source record, its predecessors, the entire target accounting
history and the reviewed plan under one recovery lock. An unrelated intervening
change can make a new plan stale; prepare another plan after reviewing the
current state. No editable JSON field or digest alone grants permission to write.

An exclusive, durable `.apply` file is created before the financial mutation
attempt. The same operation file cannot launch a second apply. After a connection or output
failure, inspect it rather than repeating the command:

- `recorded`: the exact local operation exists, its request and plan match, and
  its source-record mapping has been reconstructed.
- `not_recorded`: the exact operation was absent after acquiring the recovery
  lock. This does not authorize retrying an external model or tool request.
- An inspection error is not proof of either outcome.

A newly prepared plan for an already recorded source fact reports
`already_recorded` and the actual original `operation_id`. Its
`requested_operation_id` may differ. Applying this plan reads the existing
outcome and reports `newly_recorded: false`; it does not create a receipt for the
new ID or charge again. Later corrections do not invalidate that mapping.

The source and target may have different financial revisions, including because
only one went through quarantine. Their relationship is bound to the signed
archives and immutable target changes. Source organization totals are never
copied over the target's totals.

## Resolve a quarantined fact

Quarantine here means the fact could not be added without overflowing an integer
accounting total. It is not a request to raise the task budget. The recorded
invoice remains immutable. Resolving it does not clear the financial hold or
resume historical work.

Use the target's `quarantine_id` from the accounting outcome, together with the
original source evidence identifier. For a quarantine already present in the
original backup, use that backup's evidence and quarantine identifiers. The
command verifies their full archived records; it accepts no replacement amount.

```sh
platform recovery accounting-plan \
  --source-intent-file "$PRIVATE/source-stop.json" \
  --target-config "$PRIVATE/target.json" \
  --organization-id "$ORGANIZATION_ID" \
  --reservation-id "$RESERVATION_ID" \
  --source-evidence-id "$SOURCE_EVIDENCE_ID" \
  --mode resolve --quarantine-id "$TARGET_QUARANTINE_ID" \
  --out "$PRIVATE/resolution-plan.json"

platform recovery accounting-apply \
  --intent-file "$PRIVATE/resolution-plan.json" \
  --confirm-record-financial-fact

platform recovery accounting-inspect \
  --intent-file "$PRIVATE/resolution-plan.json"
```

If the totals still cannot represent the change, apply returns
`quarantine_retained`, `newly_recorded: false` and confirms rollback. No financial
operation receipt is created. The one-use attempt file remains: after inspecting
the outcome and recording any authentic correction to another liability,
prepare a new resolution plan. Do not delete the attempt file to repeat an old
command. A plan prepared before an intervening accounting change is stale.

Resolution can use an unresolved fact in the original backup or a captured fact
already recorded on the target. The source and target may differ: the target
may quarantine a fact the source settled directly, or resolve a fact the source
still holds. The source's full history and the target's exact provenance remain
checked in either case. A new plan for an already resolved quarantine points to
its original resolution receipt, even after a later correction.

## Current limits

A signature establishes that the fact was recorded by the trusted source; it is
not an independent verification of the provider's invoice. This path accepts
new facts contained in the captured archive. Facts arriving after capture need
a separate authenticated input path, which is not implemented here.

Overflow preserves the fact in quarantine and keeps the hold. Resolving a
quarantine uses its existing fact; facts already present in the original snapshot
are never reinserted as new invoices.

`accounting-plan` and `accounting-review` can be used after earlier accounting
operations. The older `reconciliation-plan` still requires the original target
baseline. Review is read-only and does not clear external-window or new-work
requirements.

Historical actions, task versions, events and execution permissions remain
unchanged. Financial holds remain sticky. External request coverage, fresh
ordinary credentials and safe permission for new work remain separate unfinished
recovery requirements. These commands do not constitute full restore support.

## Checked candidate

At `e84a80236e5f1e20150bb08802bc23ca005cd03c`, the
[full recovery checks](https://github.com/ArtAndrew/RunVerge/actions/runs/41419498893)
passed source-fact ordering, revision translation, stale-plan refusal, replay
after correction and real database connection loss before and after COMMIT.
The command tests used fresh child processes for plan/apply/inspection, including
lost stdout, one-use attempt refusal and a modified plan that pointed to another
fact's receipt. They used synthetic facts and disposable PostgreSQL databases.

The [Linux checks](https://github.com/ArtAndrew/RunVerge/actions/runs/41419505453)
passed installation, ordinary result persistence, restart and the existing held
evidence/reconciliation workflow on amd64 and arm64. They did not perform the new
financial command sequence through an installed package across a database
restart. No real model or provider was called. Independent operator usability,
full restoration and release readiness are not established by these checks.

At `f00f64c7b6eaeb9fdc9f54d562f22fd98bc70545`, the
[full recovery checks](https://github.com/ArtAndrew/RunVerge/actions/runs/41424296535)
also passed source-bound quarantine resolution: original invoices and upward
adjustments, a direct source settlement quarantined on the target, and a source
that still had an unresolved quarantine. They checked retained/no-receipt
outcomes, stale plans, incorrect selections and replay after later correction.
Actual database connection cuts before and after COMMIT were followed by fresh
inspection. Host command tests checked plan/apply output loss, a one-use attempt,
mode substitution refusal and replay in separate child processes.

At `79cdd8c235b7998b0551f225fee7e314aa77f7ae`, the
[installed Linux checks](https://github.com/ArtAndrew/RunVerge/actions/runs/41429088430)
passed on amd64 and arm64 with the same source digest. A separate test-only
helper created synthetic source facts through ordinary storage APIs; it is not
included in the runtime image. The installed CLI then recorded an invoice with
lost stdout, inspected it after a target database restart, refused reuse of the
same attempt file, recorded a correction and returned the original invoice
receipt on replay. Independent database reads confirmed final totals, no duplicate
facts or batches, unchanged execution records and continued recovery hold.
Both temporary environments were removed successfully.

The installed drill covers invoice/correction handling. Quarantine resolution
was checked in the storage and host-process tests above; it was not exercised
through the installed package across a database restart. No provider was called.
These checks do not establish full restoration, permission for new work,
macOS recovery, independent operator usability or release readiness.

The [SQL026 integration check](https://github.com/ArtAndrew/RunVerge/actions/runs/41590459655)
at `8b0a152d112e009f54d94b98656393b21babbc98` used the actual pinned SQL025 CLI to
create archives and populate a held target with invoices, quarantine and
corrections. Both cases—with and without a prior recovery resolution—passed the
upgrade. Existing receipt bytes and their V1 commitments remained unchanged;
replay returned the original receipts. New resolution and correction used the
explicit V2 profile, and review attributed the mixed history while preserving the
execution hold. This establishes that specific populated development-schema path,
not an upgrade from the released alpha.6/SQL022 package.
