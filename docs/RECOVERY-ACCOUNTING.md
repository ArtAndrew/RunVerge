# Record captured financial facts — development candidate

These trusted-host commands record a financial fact from a verified later source
archive into an already imported, held target. They do not restart tasks, contact
a provider or enable new work. They are not part of the alpha.6 download.

Complete [evidence ingestion](RECOVERY-EVIDENCE.md) first. Use the original and
later archives, their independently selected keys and the same private source
and target selectors. The operator needs access to those protected files and the
target database. This is not an ordinary Workspace or tenant API operation.

## Select and review a fact

Before recording changes, use [reconciliation-plan](RECOVERY-RECONCILIATION.md)
to review the original and later snapshots. Entries in `new_facts` whose `table`
is `cost_evidence` identify source facts by `record_id`, `organization_id` and
`reservation_id`. Keep this report private.

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

An exclusive, durable `.apply` file is created before the financial mutation attempt. The same
operation file cannot launch a second apply. After a connection or output
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

## Current limits

A signature establishes that the fact was recorded by the trusted source; it is
not an independent verification of the provider's invoice. This path accepts
new facts contained in the captured archive. Facts arriving after capture need
a separate authenticated input path, which is not implemented here.

Overflow preserves the fact in quarantine and keeps the hold. Quarantine
resolution is tested in the storage layer but has no host command yet. Facts
already present in the original snapshot are not reinserted as new facts.

`accounting-plan` can be used after earlier accounting operations. The broader
`reconciliation-plan` still requires the original target baseline, so retain its
initial source-fact report. A complete follow-up review interface is unfinished.

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
