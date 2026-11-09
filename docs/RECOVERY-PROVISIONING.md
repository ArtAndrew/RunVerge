# Create an isolated recovery target — development candidate

These host commands create a new PostgreSQL database, install the trusted schema
and hold it for an explicitly selected backup. They are not in alpha.6. Use
synthetic data while evaluating the candidate. They do not overwrite or modify
the source, import business data, stop source agents, reconcile later costs or
enable ordinary work.

Use a trusted PostgreSQL 18 primary and an explicit **operator-only superuser**
connection to a maintenance database. Application processes must use separate,
restricted database roles and must not have these operator credentials. This
boundary does not stop a superuser, a hostile DBA, trusted background extensions
or a host administrator. The database endpoint and its configuration are trusted;
this is not a distributed clone/failover ownership protocol.

## Plan and create

Follow the private-file, key and independently recorded receipt requirements in
[archive recovery](RECOVERY-ARCHIVES.md). The maintenance JSON contains an explicit
`database_url`, without `search_path`. Keep it and every generated file outside the
backup directory and any source repository. The selected backup must use the
current trusted schema or an explicitly recognized SQL023, SQL024 or SQL025 archive profile. The
new target always uses the current embedded schema. This candidate compatibility
path passed the bounded checks in [evidence recovery](RECOVERY-EVIDENCE.md);
it does not convert an alpha.6/SQL022 backup.

```sh
platform recovery provision-plan \
  --admin-config "$PRIVATE/maintenance.json" \
  --backup-dir "$PRIVATE/backup-001" \
  --keys-file "$PRIVATE/archive-keys.json" \
  --staging-parent "$PRIVATE/staging" \
  --expected-receipt-sha256 "$RECEIPT_SHA256" \
  --out "$PRIVATE/provision.json"
```

Planning authenticates the selected archive, checks its framing and reads the
maintenance server identity. It does not create a role or database. Review the
returned generated database name and snapshot identity. Set `TARGET_DATABASE` to
that exact name and apply within 15 minutes, without restarting the primary:

```sh
platform recovery provision-apply \
  --intent-file "$PRIVATE/provision.json" \
  --expected-database "$TARGET_DATABASE" --confirm-create-target
```

The command saves a one-use attempt marker and private target connection before
creating anything in PostgreSQL. It creates a distinct owner role without login,
password, role memberships or elevated privileges. The new database initially
rejects all connections. Public database privileges are removed before operator
connections are allowed; the connection limit remains zero. PostgreSQL exempts
superusers and background workers from this limit, so it is not a universal
network fence. See [PostgreSQL's database-creation rules](https://www.postgresql.org/docs/18/sql-createdatabase.html).

The operator target connection is checked against the exact database identity and
a fresh backend observation before any migration. Reconnecting repeats that check.
Only embedded migrations are installed; archive SQL is never executed. The command
saves the exact target intent before preparing the recovery hold.

On success, `empty_held` means the isolated target is ready for a separate import:

```sh
platform recovery archive-import --backup-dir "$PRIVATE/backup-001" \
  --keys-file "$PRIVATE/archive-keys.json" --staging-parent "$PRIVATE/staging" \
  --target-config "$PRIVATE/provision.json.target.json"
```

Use that same target file with the [recovery reader](RECOVERY-READING.md). It
contains privileged operator access, not an application connection or user login.
Never pass it to an ordinary worker or expose its contents in logs.

## Inspect an interruption

Do not repeat apply, remove its marker, recreate or delete a database automatically.
Inspect from a new process:

```sh
platform recovery provision-inspect --intent-file "$PRIVATE/provision.json"
```

| Observation | Meaning |
|---|---|
| `not_created` | Neither planned object was observed. An in-flight or uncertain attempt still does not authorize a retry. |
| `owner_created` | The exact owner exists; database creation has not been confirmed. |
| `database_closed` | The exact database exists and connections remain disabled. |
| `database_restricted` | The database has the expected owner, markers, locale and restricted access. This does not prove migration or hold completion. |

Inspection is read-only and can observe the same trusted primary after a restart.
Creation requires the server boot captured in the plan. Replacing an endpoint with
a clone is outside this profile. Unexpected ownership, grants or markers refuse
inspection rather than adopting objects that happen to share a name.

The private files explain how far initialization reached:

- `provision.json.apply`: an attempt began; no automatic second attempt is allowed.
- `provision.json.database.json`: operator-only connection saved before database
  creation. If migrations completed, use `target-inspect --database-config` with
  this file to inspect the application installation.
- `provision.json.target.json`: exact installation/restore/backup selector saved
  before hold preparation. Use `target-inspect --target-config` or `import-inspect
  --target-config` with this file. Its presence alone does not prove a held or
  committed state.

A database creation acknowledgment can be lost independently of application output.
Partial objects remain available for operator investigation. No command here
repairs a partial creation, resumes an old queue or declares recovery complete.

## Verification of this candidate

The disposable PostgreSQL run below covers the original provisioning path. A
later local Linux ARM64 candidate exercised source retirement, held import,
scoped admission and a fresh API task with a separate worker. The same drill
restored a second copy and verified the task result, artifact and cost. It used
synthetic data and a newly created Docker Compose project.

[The disposable PostgreSQL run](https://github.com/ArtAndrew/RunVerge/actions/runs/41316164535)
passed creation, authenticated import, exact result reading with fresh access,
revocation and retained execution hold. It also checked raw connection denial for
restricted application credentials, isolation checks on reconnect, and wire loss
before and after the database creation statement. Commands ran through compiled
test helper processes. This is not an installed-package restore drill, a test of a
specific old application binary, a macOS deployment check or permission to enable
new work. The later source change only corrects this test's explanatory comment.

## Use a drained capture as a new baseline

For a controlled handoff, first complete the bound source shutdown and V2 capture
described in [source shutdown](RECOVERY-CONTAINER-STOP.md). Keep exclusive control
of that source host. This path selects the **later, drained archive** for a new
held database. It does not change an existing recovery target or reconcile an
older backup's missing interval.

Use the exact late directory, key file and staging directory saved in the source
plan. `LATE_RECEIPT_SHA256` is `link.evidence_receipt_sha256` from its authenticated
`.capture` file. Keep the source plan, `.capture`, `.capture-state`, final stop
receipt and both archives available; all private authority and output files must
remain outside both archives.

```sh
platform recovery provision-plan \
  --admin-config "$PRIVATE/maintenance.json" \
  --backup-dir "$PRIVATE/late-evidence" \
  --keys-file "$PRIVATE/archive-keys.json" \
  --staging-parent "$PRIVATE/staging" \
  --expected-receipt-sha256 "$LATE_RECEIPT_SHA256" \
  --handoff-source-intent-file "$PRIVATE/source-stop.json" \
  --handoff-source-stop-file "$PRIVATE/source-stop.json.source-stop" \
  --out "$PRIVATE/handoff-provision.json"
```

Review the new database name, then use `provision-apply` as above with this new
plan. The command authenticates the retained capture and final stop, checks that
the same source processes remain stopped, and commits the provenance digest to
the new database and owner markers. It rechecks the source before creation.
Version 1 capture links cannot be used for this profile.

When the source was [retired for one destination](RECOVERY-CONTAINER-STOP.md#retire-the-source-for-one-recovery-destination), use the selected maintenance
primary, database and account. The plan reuses the original `restore_id` and
checks the destination's current WAL timeline. A second plan cannot allocate a
different database under that retired handoff. The retained source observation
is provenance for a held import. Enabling work requires fresh authority and
current checks.

Import only the selected late archive:

```sh
platform recovery archive-import \
  --backup-dir "$PRIVATE/late-evidence" \
  --keys-file "$PRIVATE/archive-keys.json" \
  --staging-parent "$PRIVATE/staging" \
  --target-config "$PRIVATE/handoff-provision.json.target.json"
```

Import checks the physical target through the saved provisioning plan, rechecks
source stop, and preserves the captured history and obligations. Removing the
handoff fields from the target file does not permit generic import into this
database. No historical row is turned into current authority and no unresolved
reservation is released. `committed_held` still denies ordinary access and work.

After lost output, use `provision-inspect` and `import-inspect`; do not repeat
creation. Read-only provisioning inspection does not require the source to be
reachable or still stopped. It reports the target state, not permission to
continue. Fresh credentials require the separate admission below. Use
[reviewed provider statements](RECOVERY-USAGE.md#post-a-reviewed-final-statement)
for late financial reconciliation on an admitted target.

### Review the held handoff

Use the bound Linux Compose maintenance host after the late baseline import.

```sh
platform recovery handoff-review --target-config "$PRIVATE/handoff-provision.json.target.json"
```

The review authenticates both archives and checks the exact stopped source
generation before and after reading the target. When the final stop includes
a signed retirement seal, the review binds the PostgreSQL admission and data
proofs to that generation and the reserved target. Earlier stop receipts
still support held review. It checks the physical target,
import receipt, historical row identities, sequence floors and current
accounting head. It lists recorded actions in both snapshots and current open
reservations, including unknown costs with a zero bound. The source stop
observation time is reported separately from the evidence digest so a fresh
review can be compared with an earlier one.

Keep the report private. Its scope is the observed stopped installation and
the data verified while the target was held. Recorded dispatch describes a
local admission. An external response, effect or charge requires separate
evidence. The report lists the remaining prerequisites for enabling new work.

### Reconcile an independently recorded external window

A sealed handoff review includes `peer_scope` and
`peer_minimum_through_utc`. Use them with an independently trusted observer
that controlled the source installation's full outbound execution boundary
from the original cut through shutdown. The observer must account for requests
already in flight and confirm when the original sender's egress closed.
Logs from one provider cover that provider's traffic. This profile requires
the complete installation boundary, including tool preflight and discovery.

Keep the observer's public key, signed journal and new output in a private
directory outside both archives. Pin the public key out of band. The trust
JSON has `version` set to 1, the exact `scope` copied from `peer_scope`,
and `public_key` set to the base64 encoding of the observer's 32-byte
Ed25519 key. RunVerge requires a key distinct from the source stop signer.

The compact journal envelope has `journal` followed by `signature`. The
journal fields in wire order are `version` (1), `profile`
(`exclusive-source-egress-v1`), `scope`, `egress_fenced_at_utc`,
`covered_through_utc`, `egress_proof_sha256`, and `records`. Use UTC
RFC3339 timestamps and cover at least `peer_minimum_through_utc`. The egress
proof digest refers to the observer's retained evidence of source isolation
and the closed interval. Sign the exact compact journal JSON with Ed25519,
prefixed by the UTF-8 bytes of `RunVerge independent egress journal v1`
and one NUL byte. Base64-encode the signature. Preserve the scope field
order printed by the review and the original signed bytes.

Each normalized record has `family`, `organization_id`, `record_id`,
`dispatch_sha256`, `evidence_sha256`, and `disposition`, in that order.
Match the observer's retained evidence to the exact entries in
`target_review.execution_window.records`. Use `effect_observed`,
`no_effect_confirmed`, or `unknown` for the observer's disposition.
For the same tool call or model probe, its action and specialized record
must refer to the same effect evidence. Represent an independent observation
without an exact local match as `unmatched` with an empty `dispatch_sha256`.
Provide the organization only when the observer can attribute it. Missing
attribution holds the entire restore. The journal accepts up to 2,048 records
in a private file of up to 1 MiB.

```sh
platform recovery handoff-review \
  --target-config "$PRIVATE/handoff-provision.json.target.json" \
  --peer-journal "$PRIVATE/observer-journal.json" \
  --peer-trust "$PRIVATE/observer-trust.json" \
  --peer-receipt-out "$PRIVATE/peer-review.json"
```

The report shows scope blockers and unmatched evidence. It checks a marked
action against the current verified cost ledger, retaining open liabilities
even for a zero bound. The new private receipt binds this result to the
signed journal, import and accounting head. Keep the journal and trust file
with that receipt. Financial settlement and current execution authority
remain separate operations. The recovery target stays held.

### Enable new work for reviewed organizations

Use the exact sealed handoff, observer files and peer receipt from the review.
Select the organizations whose external window is covered and whose financial
hold is clear. Confirm the current owner of each selected organization before
writing a private scope file. Include one owner per selected organization.

```json
{"owners":[{"organization_id":"ORG_ID_FROM_REVIEW","display_name":"Reviewed owner"}]}
```

For an owner who uses SSO, include `oidc_issuer` and `oidc_subject` in that
entry. Obtain both from the trusted identity provider and match the organization's
login policy. The user starts a fresh SSO session. Each organization still
requires an explicit membership in this request.

```sh
platform recovery handoff-enable \
  --target-config "$PRIVATE/handoff-provision.json.target.json" \
  --peer-journal "$PRIVATE/observer-journal.json" \
  --peer-trust "$PRIVATE/observer-trust.json" \
  --peer-receipt "$PRIVATE/peer-review.json" \
  --scopes-file "$PRIVATE/reviewed-owners.json" \
  --operator-reference "CHANGE_RECORD" \
  --private-output "$PRIVATE/admitted-runtime.json" \
  --confirm-current-authority
```

The new output contains the restricted runtime database URL and, for
bearer-based owners, fresh tokens by organization. Keep it private.
RunVerge saves these credentials before attempting admission. Confirm the
committed outcome before configuring the API and worker with
`runtime_database_url` as their private `DATABASE_URL`. Configure the trusted
OIDC issuer and TLS separately for SSO access.

```sh
platform recovery handoff-enable-inspect \
  --private-output "$PRIVATE/admitted-runtime.json"
```

`recorded` confirms that the exact request committed. Inspect the same file
after interrupted output. The fresh login can run new work in the reviewed
organizations. Historical tasks and sessions stay fenced, and outstanding
liabilities remain available for accounting.
