# Reading a held recovery target — development candidate

These commands are part of an unreleased recovery component. They read a target
already imported and held by the matching-schema recovery importer. They do not
create backups, import archives, upgrade an installation, stop the source server,
reconcile later costs or enable new work. The published alpha support limits still
apply. Use synthetic data while evaluating this candidate.

The commands run on the trusted recovery host. They are not a remote login service
or a team-access feature. They never use `DATABASE_URL` implicitly and never start
workers or bootstrap an owner from the backup.

## Prepare current access independently

The host operator must establish which people may read which organizations using
current information outside the backup. An old role or login session in a backup
is not that evidence. `--operator-reference` records the operator's review reference;
it is an attestation, not an automated identity-service verification.

Use a dedicated local directory with mode 0700, owned by the operating-system
account running the recovery commands. Its ancestors must prevent another user
from replacing its pathname. ACLs must not grant other users access. This profile
checks Unix ownership, modes and file identities; it does not enumerate ACLs or
certify the host. macOS and Linux local filesystems are the initial profile.
Network, cloud-synced and shared directories are not covered.

Keep this directory outside archives, repositories and release packages. It will
contain fresh signing material, current policy and short-lived bearer tokens.
The backup importer never restores these files. A trusted owner/root can alter or
roll back files; this component does not protect against a hostile host owner.

Prepare two private 0600 JSON files in that directory. `target.json` contains the
explicit target database connection and the exact import receipt identity:

```json
{
  "database_url": "PRIVATE_TARGET_POSTGRESQL_DSN",
  "target_installation_id": "inst_FROM_TARGET",
  "restore_id": "restore_FROM_IMPORT",
  "backup_id": "bak_FROM_IMPORT",
  "source_id": "inst_FROM_SOURCE_RECEIPT",
  "cut_utc": "EXACT_IMPORT_CUTOFF",
  "schema_sha256": "EXACT_SCHEMA_DIGEST",
  "snapshot_sha256": "EXACT_SNAPSHOT_DIGEST"
}
```

The values above are placeholders. Use the actual receipt; do not invent or infer
IDs from the source installation. The DSN must specify one host, port, database,
username, password and `sslmode=disable` or `sslmode=verify-full`. Verification
requires an explicit absolute `sslrootcert` path. Optional `search_path` selects
one schema. Service files, multi-host fallbacks, arbitrary connection options and
nonempty `PG*` environment settings are refused. Client-certificate authentication
is not part of this profile. `scopes.json` records the newly reviewed grants:

```json
{"scopes":[{"subject":"reviewer-alice","organization_id":"org_FROM_REVIEW"}]}
```

A subject is a capability recipient label. Possession of its token grants reads;
the token does not prove the identity of the human presenting it.

## Initialize and read

In the examples, `PRIVATE` is the absolute path to your private parent directory.
`authority` must not exist before initialization.

```sh
platform recovery access-init --authority-dir "$PRIVATE/authority" \
  --target-config "$PRIVATE/target.json" --scope-file "$PRIVATE/scopes.json" \
  --operator-reference "review:current-access" --confirm-current-authority

platform recovery issue-read-token --authority-dir "$PRIVATE/authority" \
  --subject reviewer-alice --organization-id org_FROM_REVIEW \
  --lifetime 5m --out "$PRIVATE/read.token"

platform recovery list-tasks --authority-dir "$PRIVATE/authority" \
  --target-config "$PRIVATE/target.json" --token-file "$PRIVATE/read.token" \
  --organization-id org_FROM_REVIEW --limit 20

platform recovery read-result --authority-dir "$PRIVATE/authority" \
  --target-config "$PRIVATE/target.json" --token-file "$PRIVATE/read.token" \
  --organization-id org_FROM_REVIEW --candidate-id CANDIDATE_FROM_TASK
```

Initialization verifies an exact committed held target. Token creation never
prints the token or overwrites an existing file. Store token files outside the
authority subdirectory. The maximum lifetime is 15 minutes; the default is 5 minutes.
An invalid, expired or revoked permit returns no result content.

Task pages include `next_after_id` when more data exists; pass it as `--after-id`.
Results identify the cutoff and an unresolved interval after it. They are
historical untrusted content. Reading a saved result or checking its digest does
not verify its factual claims or prove that no later work happened.

## Change or revoke access

Inspect before making a change, then replace the complete reviewed scope list:

```sh
platform recovery access-inspect --authority-dir "$PRIVATE/authority"
platform recovery access-replace --authority-dir "$PRIVATE/authority" \
  --expected-epoch 1 --scope-file "$PRIVATE/scopes.json" \
  --operator-reference "review:updated-access" --confirm-current-authority
```

Use the actual inspected epoch. Each replacement advances the epoch and rotates
the signing key, invalidating every earlier token. `{"scopes":[]}` explicitly
revokes all scopes. Issue new token files for scopes that remain authorized.
Replacement waits for cooperating reads already in progress; it cannot retract
data previously returned. Each new process reloads the durable policy.

## A change stopped before reporting success

Do not repeat initialization, remove a lock/intent file or guess that revocation
succeeded. Inspect the exact authority directory. A pending operation blocks new
reads and token issuance. Review its next scope list and exact operation ID, then:

```sh
platform recovery access-resolve --authority-dir "$PRIVATE/authority" \
  --operation-id EXACT_PENDING_OPERATION_ID
```

Resolution completes that recorded change with its original new key; it does not
silently create another grant. If inspection already shows the expected operation
as active, the change is recorded and there is no pending operation to resolve.
Malformed or incomplete state remains closed and requires operator investigation.

The implementation syncs a pending intent before changing active policy and syncs
active policy before clearing the intent. A failure before the intent is durable
does not start active replacement and does not acknowledge revocation. Process
crash tests are not a physical power-loss guarantee. Host storage guarantees and
external source isolation remain separate requirements for complete recovery.
