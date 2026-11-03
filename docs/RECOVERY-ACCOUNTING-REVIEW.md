# Review recovery accounting — development candidate

Use this trusted-host command before or after recording financial changes. It
shows the authenticated original backup, the captured source snapshot and the
current held target together. It does not write financial records or enable
execution. It is not part of the alpha.6 download.

Complete [evidence ingestion](RECOVERY-EVIDENCE.md) first. Use the same protected
source and target selectors, archive directories, keys and staging directory:

```sh
platform recovery accounting-review \
  --source-intent-file "$PRIVATE/source-stop.json" \
  --target-config "$PRIVATE/target.json"
```

Keep the output private: it includes organization, reservation and financial
record identifiers across the installation. This is an operator command, not a
Workspace or tenant API.

## Read the three views

Each organization and reservation has three separate fields:

| Field | Meaning |
|---|---|
| `original` | What the selected backup contained |
| `captured` | What the later authenticated source snapshot contained |
| `target` | What is currently stored in the restored database |

A null view means the organization or reservation is absent there; it does not
mean a zero balance. Monetary values are exact decimal strings in micro-USD.
A null actual amount means no actual amount is recorded; read it together with
`status`. It differs from a recorded `"0"`. Reservations whose status is `unknown`
are counted even when their reserved amount is zero.

For example, an original reservation can have an unknown actual amount, the
captured source can contain a final correction to 3 micro-USD, and the target
can still show an earlier invoice of 7 micro-USD. Recording the correction changes
the target position. The historical invoice row keeps its original amount of 7.

## Find the next financial step

`facts` separates where a record came from, whether it exists on the target and
whether its quarantine is still pending:

- `origin`: original backup, captured source, or an unattributed target fact.
- `recording`: `imported`, `recorded`, `not_recorded` or `unattributed`. A recorded
  fact can still be quarantined; this field alone is not proof of settlement.
- `target_evidence_id` and `target_quarantine_id`: the actual target identities,
  which can differ from the source identities.
- `fact_operation_id` and `resolution_operation_id`: the actual recovery
  receipts, where their source mapping has been independently reconstructed.
  A resolution already in the original backup is marked `resolution_origin:
  "original"` and has no invented recovery operation.

Use `next_step` as a review hint:

| Value | Next step |
|---|---|
| `prepare_fact_plan` | Prepare a separate plan for this source evidence ID |
| `await_predecessor` | Review and record an earlier fact for this reservation first |
| `resolve_pending_quarantine` | Review the target's pending quarantine before the next fact |
| `prepare_resolution_plan` | Prepare a resolution plan with the target quarantine ID |
| `unmapped_scope` | This scope was absent from the original backup; it cannot be introduced through accounting apply |
| `review_unattributed_operations` | Some target receipts have no verified source mapping; inspect them before proceeding |
| `unsupported_history`, `unsupported_catalog`, `unsupported_target_state` | This path cannot establish a supported next plan |
| `none` | No new financial command is suggested for this fact |

The [accounting command guide](RECOVERY-ACCOUNTING.md) describes plan, apply and
inspection. A hint does not guarantee that a later apply will succeed: another
accounting operation can change the head or capacity. Apply always rechecks its
inputs. Neither `none` nor a recorded receipt permits replaying an external model
or tool request.

## Check what remains unresolved

`receipts` includes every accounting batch up to the reported head, not only the
ones that matched source facts. Its `attribution` is `source_fact`,
`source_resolution` or `unattributed`. A structurally valid mutation receipt is
not automatically proof of an authentic invoice. Unattributed operations stay
visible and suppress fresh planning hints.

`blockers` also retains unknown outcomes, pending quarantines, missing source
scope, unsupported histories and unproven external-window coverage. The report
remains `reconciliation_state: "pending"` with `execution_held: true`. Accounting
review cannot establish facts arriving after capture, fresh ordinary authority
or safe permission for new work.

`review_sha256` binds the three views, their source receipts, capture link, target
identity, accounting head, all receipt attributions and reported states. Without
new data, a fresh process or target database restart should produce the same
digest. A changed digest means the review inputs or results changed, not that
new work has been approved.

The reader checks the complete target baseline and source mappings, then rolls
back its transaction. Invalid history, cancellation or exhausted limits produce
an error rather than a partial success report. Resource bounds are not a
performance benchmark. Full restore and release readiness remain separate.

## Checked scope

[Recovery validation](https://github.com/ArtAndrew/RunVerge/actions/runs/41438200398)
passed at `701f9976804e7d3c99c523ccfba1561497da14e2`. It covers review before
and after an invoice and correction; original and newly recorded quarantine
resolutions; an insufficient-capacity attempt that leaves the review unchanged;
unattributed receipts; cancellation, limits and modified receipt refusal; and
repeat review from fresh host processes.

[Installed Linux validation](https://github.com/ArtAndrew/RunVerge/actions/runs/41434189075)
passed on amd64 and arm64 at `e516c0569d86508c620847b215458f42cd7134bf`.
The installed command returned the same review digest after the target database
restarted. A subsequent correction changed the target total from 7 to 3 micro-USD
while the original invoice retained its amount and operation identity. Both
disposable environments were removed. The later commit changes test grouping
only; runtime code is identical.

The installed check covers the invoice/correction path, not every quarantine
case. These checks use synthetic financial facts and make no real model calls.
They do not prove final provider billing, post-capture evidence coverage, fresh
user authority or permission to run new work after recovery.

## Recorded outbound operations — development candidate

The current candidate returns review version `2`, algorithm
`held-accounting-execution-review-v2`. Its `execution_window` contains a versioned
inventory of the original and captured snapshots. This additional input changes
the review digest; do not compare a version 1 digest with a version 2 digest as
though the report formats were identical.

Five record families are always listed, including zero counts:

| Family | Recorded information | `related_ids`, in order |
|---|---|---|
| `action` | Model/tool/other action and its reservations; `kind` shows the category or `mixed_or_unbound` | Reservation IDs, sorted |
| `tool_execution` | Tool occurrence and its dispatch tuple | Action ID, reservation ID |
| `configuration_verification` | Model connection check or MCP discovery admission | Plan ID, target resource ID |
| `webhook_attempt` | One delivery attempt with its envelope/profile binding | Delivery ID |
| `reservation_without_action` | A financial position without an action, including legacy fixture history | Empty list |

These are record counts, **not numbers of independent charges or HTTP requests**.
A tool execution is also represented by its action. A model connection check uses
the ordinary model reservation. An MCP occurrence may involve several protocol
requests. The financial views above remain the source of reported amounts.

Each record is keyed by family, organization and record ID. `original` and
`captured` retain their status, reference IDs and row/identity/dispatch digests.
The comparison distinguishes:

- `captured_only` or `missing_from_capture`: present in only one snapshot,
  including terminal and zero-cost records.
- `identity_changed`: the identity projection or bound references differ.
- `dispatch_record_changed`: a previously recorded dispatch tuple changed or
  disappeared; it is not treated as a harmless missing marker.
- `dispatch_recorded_after_backup`: the original record had no dispatch marker,
  while the captured record has one. Original stored history is not rewritten.
- `dispatch_recorded_in_both`: both contain the same recorded admission.
- `no_dispatch_record`: neither contains a marker. This **does not prove that
  no request escaped**, particularly outside the supported runtime profile.

A dispatch marker records admission, not confirmed network delivery, completed
effects or a final bill. Identity projections help compare records; they do not
prove that the listed processes were the only senders throughout the interval.
OIDC/discovery/JWKS traffic is outside this business-operation inventory. A final
source-stop receipt is not rechecked by this accounting command.

`coverage_proven`, `source_stop_rechecked` and `new_work_allowed` remain false.
Global recovery blockers and all existing financial holds remain in place. The
inventory is computed from the verified archive bytes under the same target and
accounting-head checks as the review; it is included in `review_sha256`. No source
record or target obligation is modified. Limits fail the whole report rather
than return a partial inventory. Runtime validation of this new report is pending.
