# Recovery implementation contract

SPDX-License-Identifier: Elastic-2.0

Status: development candidate, 1 November 2026. **Complete restoration and
resumption of work are not implemented or accepted.** The published alpha.6
support profile remains unchanged.

The candidate contains these components:

| Component | Implemented boundary |
|---|---|
| Export and encrypted archive | Consistent bounded data-only snapshot; authenticated encryption; independently selected archive identity |
| Typed staging | Trusted schema, row and reference checks, selected execution and cost-history checks; rollback on every exit |
| Held import | Atomic same-schema import with a durable receipt and historical-row fences; no hold clearance |
| Recovery reader | Fresh target-bound, organization-scoped host capabilities; no ordinary login or worker admission |
| Database disconnection | Selected source database connection ban and session termination; does not establish external execution shutdown |
| Compose shutdown | Explicit one-use plan for a reviewed local container inventory; does not yet prove the installation binding or settle in-flight effects |

The [archive commands](../docs/RECOVERY-ARCHIVES.md),
[recovery reader](../docs/RECOVERY-READING.md),
[database disconnection](../docs/RECOVERY-DATABASE-FENCE.md) and
[Compose shutdown](../docs/RECOVERY-CONTAINER-STOP.md) document their individual
prerequisites. They are not a complete operator recovery procedure. In particular,
the database-disconnection and Compose-shutdown profiles have different deployment
boundaries and cannot simply be concatenated into a verified recovery flow.

The importer requires matching trusted migrations, including SQL023. SQL022 and
SQL023 are immutable. There is no supported conversion of an alpha.6/SQL022 archive
into the candidate's SQL023 schema. Migration, target provisioning and subsequent
work enablement need their own acceptance evidence.

Typed staging continues to report `full_semantic_validation: false`. It verifies
specific history families and records residual coverage separately; a successful
check does not prove every historical business outcome. The importer preserves
unverified history behind durable fences instead of authorizing replay. The
PG18/C profile requires trusted engine catalogues and refuses non-engine custom
types and external inbound foreign keys.

Team input validation accepts two exact serializations: the earlier input with
feedback retained in its parent context, and alpha.6's input with the parent's
feedback also copied to the child. Both retain exact parent context, selected
sources and input digest. There is no format marker to identify the producing
release. Recovery must not rewrite stored input or model transcripts to convert
between those forms.

Verified source/deployment binding, target connection isolation, post-cutoff
factual reconciliation, fresh authority for ordinary work and safe enablement
remain required below. A held import or encrypted snapshot round trip does not
satisfy these conditions.

## First supported target

Restore the complete selected installation's data into a **new, isolated
PostgreSQL database**, using the matching trusted application schema. Initially
support the native macOS arm64/PG18 profile. Do not overwrite, drop, reconnect
or migrate the source installation. A failed or ambiguous operation keeps the
target inaccessible and preserves recovery evidence.

The end-to-end acceptance scenario must include reading restored results with
fresh authority and starting explicitly authorized **new work** after source
fencing and reconciliation. An encrypted archive or a permanently read-only
restore is a useful intermediate result, but does not close the alpha's
resumable recovery condition. Previously queued work is not new work.

The first implementation can require a documented maintenance window and
verified source shutdown. That limits availability; it does not eliminate
in-flight external effects. If the interval between the snapshot and verified
source fencing is not accounted for, execution remains held. A later online
backup/provider profile needs its own tested coordination and recovery policy.

## Distinct facts and authorities

1. **Historical data:** original IDs, versions, artifact content, receipts,
   reservations, ledger entries, limits and task history remain exact.
2. **Recovery uncertainty:** an independent overlay records that the restored
   snapshot cannot establish everything that happened after its cutoff. Do not
   invent a dispatch timestamp, admitted Action, invoice or zero-cost outcome.
3. **Recovery authority:** fresh, explicitly issued access bound to this target,
   its recovery generation and an explicit organization scope. Old bearer keys,
   browser sessions, service keys, login transactions and grants never authorize
   recovery or resume. A new credential alone is not proof of current ownership.
4. **Operational admission:** current authorization, ordinary financial rules
   and recovery clearance are separate requirements. Clearing a recovery hold
   must not clear a pre-existing financial hold or release unknown reserves.

Restore cannot reconstruct post-cutoff revocations from the snapshot alone.
All restored credentials and execution grants are therefore denied, including
those which the snapshot still calls active. Fresh authority requires trusted
operator/current identity evidence. Original authority snapshots remain
available as history; they are not silently updated to current epochs.
Ordinary OIDC login/identity mappings, service-key issuance and host bootstrap
must not mint replacement authority from restored roles. They do not bypass
the independent target-bound recovery admission check.

## Recovery states and gates

Target lifecycle: `reserved → importing → held → reconciled → new_work_enabled`.
These are design states, not currently implemented wire enums. `failed` or
`outcome_unknown` never implies cleanup, retry or enablement is safe.

Before the import can commit, the target must have a durable recovery hold and
the restored entities must be identifiable as historical. Runtime admission
fails closed when this state is missing, malformed or unsupported. The target
also requires fresh database credentials and isolated connectivity. While
held, ordinary application roles have no connection/data-write path; an old
binary must be tested and denied by that external boundary. A new password
alone does not establish this isolation or make an old binary honor new guards.

The hold applies **before claiming or mutating historical work**, not just
before external I/O. It covers model/tool/configuration/webhook workers,
automation, leases, reapers, cancellation, retry, reserve release, cached
mutation replay and trusted accounting ingestion. Zero-tariff local calls are
included. Read-only recovery and explicit reconciliation operations use their
own narrow authority and do not run these normal background paths.

This distinction is necessary in the present source: `ClaimToolExecution` can
call `stopToolUnsentTx` after an authority denial, while `ClaimModel` can block
an operation. A missing sent marker in an old snapshot cannot establish that
an action was never sent after the cutoff. Merely restoring an organization
with `active=false` or `financial_hold=true` is therefore insufficient.

Clearing the installation or organization recovery hold never reactivates an
old task, lease, consent, automatic continuation, configuration verification
or webhook delivery. Historical work stays excluded from ordinary mutation
and dispatch even after new work is enabled. Resubmission would require its
own later reviewed protocol, with evidence that an unknown effect is not
being duplicated. The alpha does not provide a blanket “resume old queue”.

## Snapshot, archive and trust boundaries

Use one consistent database snapshot and an exact ordered schema identity.
Export data only; restore into schema created from trusted embedded migrations.
Do not execute SQL, trigger definitions, owner changes, extensions or program
commands supplied by a backup. A signature on an archive does not make database
code inside it safe to execute.

The implementation must define a complete table/column/type inventory,
per-entry size and content hashes, limits on total decoded size, row counts,
sequence high-water marks and references to required external keys. Database
sequences are not ordinary MVCC rows: their capture/restore semantics must be
tested explicitly. Binary COPY, if chosen, must have an exact supported PG/type
compatibility policy; portable compatibility is not inferred from file hashes.

Historical records can legitimately violate *current admission* triggers
(for example a version greater than one at insertion). During import, only
trusted schema-derived guards may be temporarily suspended. Never take their
SQL or identifiers from the archive. Restore and verify all constraints and
guards, validate references and record the recovery hold in the same atomic
data-import transaction. Re-enabling a trigger does not retroactively validate
rows loaded while it was disabled. Explicitly validate trusted foreign-key
relationships and accounting/identity projections before commit; orphan and
inconsistent-counter fixtures must fail. Use no superuser execution of archive
content. Event/cursor consumers also need a recovery-generation reset or
binding: restoring a sequence cannot recover lost post-cutoff positions or
prove that a cursor from the old installation remains valid.

Use a maintained standard authenticated encryption format; select and pin
the implementation after reviewing its official documentation. Authenticate
the publisher/source independently of encryption: a recipient's public key
is not proof of who encrypted an archive. Pin a trusted signing key or external
authenticated digest outside the backup, with a documented key rotation and
revocation policy. Do not trust a signer supplied only by that same archive.

Authenticate and validate the complete input before database mutation. Use
bounded streaming and owned private temporary storage, not unbounded memory
or plaintext in public output. Reject truncation, duplicate/unknown entries,
traversal, links, unexpected file types and unauthenticated trailing content.
Never place raw model/API secrets, database passwords or identity seeds in
public manifests/logs. Key references are not a backup of the keys themselves.

## Target isolation and failure handling

Creation uses a fresh target identity and a new destination; no overwrite or
automatic destructive cleanup. The trusted restore process must not attach to
a database merely because it has the expected name. Record and verify target
ownership and recovery identity. Lost CREATE/COMMIT acknowledgments produce
an inspectable unknown outcome; they do not authorize DROP, a second import,
reusing an old target or starting API/worker processes.

Source fencing must be verified outside the restored snapshot. A declaration
inside the backup, a dead PID without process identity, a lease timeout, an
expired HTTP deadline or a new target generation alone does not fence another
host. Existing requests may have completed remotely after a client stopped.
The first profile requires operator evidence and tests for its exact single-host
boundary; it makes no distributed fencing claim.

## Reading, reconciliation and enabling new work

A recovery reader has a narrow allowlist of read-only operations and a fresh
target-bound, organization-scoped credential. Reusing the ordinary `viewer`
role alone does not implement this boundary. Returned content is untrusted
historical material. The recovery UI must display the snapshot cutoff and
unresolved interval prominently.

Reconciliation is an explicit trusted-host operation, separate from reading
and from ordinary user writes. It appends evidence without external sends or
inferred reserve release. Authentic post-cutoff facts that cannot be mapped
to restored IDs remain preserved as unresolved recovery evidence and hold
their affected scope; do not manufacture matching rows or discard the facts.
Known authentic late invoices/receipts can be appended and reconciled through
this trusted path while execution stays fenced. Historical execution is frozen;
the financial record is not permanently forbidden from learning later facts.

Enable new work only with verified source fencing, reconciled window coverage,
fresh current authority, checked external key references and ordinary budget
admission. The operator receives an exact reviewable report, not a generic
confirmation that silently settles unknown costs. Preserve original financial
holds and limits. Missing proof keeps the affected scope held. Record every
clearance and its evidence outside mutable cached responses.

## Required implementation evidence

| Scenario | Required observation |
|---|---|
| New target, representative populated data | Exact artifacts/history/ledger/limits; complete inventory; all trusted guards restored |
| Post-cutoff send, source journal outside backup | Zero repeated calls; missing local marker is not treated as unsent; liability preserved |
| Post-cutoff revocation | Old human/service/session/grant cannot authenticate or dispatch |
| Worker/reaper started during held recovery | No dispatch, claim, cancellation or reserve release, including zero-tariff calls |
| Hold cleared for new work | Fresh task can run; every historical queue/lease/grant remains inert |
| Missing/wrong key, tamper, truncation, extra entries | Refused before data import; no secret output or automatic fallback |
| Crash/unknown acknowledgment at create/import/commit | No overwrite, duplicate import or premature enablement; inspection resolves ownership/state |
| Source still able to execute | Target remains held; no split-brain success claim |
| Sequence and schema compatibility | Fresh IDs/cursors/generations cannot collide or rewind; incompatible input is refused |
| Independent restore drill | New installation and useful resumed workflow; measured dataset, elapsed recovery and cutoff loss |

These tests must use disposable databases and an independent reviewer. Record
first failures, source hashes and exact commands. RPO/RTO are measured for the
tested profile and volume, not promised from an empty-database timing. Do not
use a working installation for destructive development checks.
