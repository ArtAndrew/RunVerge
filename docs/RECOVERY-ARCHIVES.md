# Backup and held import — development candidate

These host commands create an encrypted database snapshot, verify a selected
backup and import it into a new, held target. They are not included in the
published alpha.5 package. Use synthetic data for evaluation. The target stays
closed to ordinary logins and workers; completing an import does not resume work.

This is the same-schema path only. The source, target and binary must use the
same trusted migrations, including migration 023. There is no 022-to-023 archive
conversion or supported upgrade here. Provision the target with trusted tools
before using these commands; they never create a database or run migrations.

## Private files and keys

Use absolute paths under dedicated, owned 0700 directories on a local macOS or
Linux filesystem. Files are 0600 and must not be symlinks or hardlinks. Protect
ancestor directories and ACLs against other users. The commands check ownership,
Unix modes and file identities, not every ACL or the trustworthiness of the host.
Shared, cloud-synced and network filesystems are outside this profile.

The examples use `PRIVATE` for that directory. Create an empty private `staging`
subdirectory there. Keep keys and target intent outside the backup directory and
outside source repositories. Keep a separate protected copy of the keys: losing
them loses the ability to recover the backup. This initial single-owner key file
combines decryption and signing material; it does not separate those roles.

Prepare `source.json` and `new-database.json` as private files containing:

```json
{"database_url":"EXPLICIT_PRIVATE_POSTGRESQL_URI"}
```

Each URI must specify one host, port, database, user, password and explicit
`sslmode=disable` or `sslmode=verify-full`. TLS verification also requires an
absolute `sslrootcert` path. An optional `search_path` selects one schema.
Nonempty `PG*` environment settings, service files, implicit defaults and
multi-host fallbacks are refused. `DATABASE_URL` is not used by these commands.
The password stays in the private file; do not place it in command arguments.

## Create and select a backup

```sh
platform recovery backup-keygen --out "$PRIVATE/archive-keys.json"

platform recovery backup-create --database-config "$PRIVATE/source.json" \
  --keys-file "$PRIVATE/archive-keys.json" \
  --staging-parent "$PRIVATE/staging" --out-dir "$PRIVATE/backup-001"
```

Both outputs must be new. An existing file or backup directory is never
overwritten or adopted. The backup directory contains `archive.rvg`,
`receipt.json` and `backup.lock`. Keep these three files together, with their
private permissions. A lock prevents cooperating readers from accepting a
backup while its writer is still active.

The successful command prints a manifest and `receipt_sha256`. Record that digest
separately from the transported backup, in a trusted record. This selects the
intended backup: a valid signature alone does not distinguish today's backup
from an older one signed by the same key. Do not calculate the expected value
from an untrusted replacement `receipt.json`.

Set `RECEIPT_SHA256` to the digest in that independent record, then:

```sh
platform recovery archive-verify --backup-dir "$PRIVATE/backup-001" \
  --keys-file "$PRIVATE/archive-keys.json" \
  --staging-parent "$PRIVATE/staging" \
  --expected-receipt-sha256 "$RECEIPT_SHA256"
```

Verification checks encryption, signature, selected identity and snapshot framing.
It does not validate every database relationship or certify the truth of saved
results. Import performs additional typed, relational and history checks.

The command profile limits plaintext to 384 MiB and encrypted payload to 400 MiB.
Creation temporarily needs space for a plaintext spool and two encrypted copies;
these limits are not a host disk quota. Plaintext staging files are private and
unlinked while open. A successful command waits for staging cleanup. This is not
a promise of secure erasure from storage or process memory.

## Prepare and import into a new target

Provision a separate empty target and prevent old binaries, workers and source
credentials from accessing it. Source isolation is an operator prerequisite;
`--confirm-new-target` records an attestation, not technical proof of isolation.
Inspect the intended target and record its installation ID:

```sh
platform recovery target-inspect --database-config "$PRIVATE/new-database.json"

platform recovery target-prepare --backup-dir "$PRIVATE/backup-001" \
  --keys-file "$PRIVATE/archive-keys.json" \
  --staging-parent "$PRIVATE/staging" \
  --expected-receipt-sha256 "$RECEIPT_SHA256" \
  --database-config "$PRIVATE/new-database.json" \
  --expected-installation-id "$TARGET_INSTALLATION_ID" \
  --intent-out "$PRIVATE/target.json" --confirm-new-target

platform recovery archive-import --backup-dir "$PRIVATE/backup-001" \
  --keys-file "$PRIVATE/archive-keys.json" \
  --staging-parent "$PRIVATE/staging" --target-config "$PRIVATE/target.json"

platform recovery import-inspect --target-config "$PRIVATE/target.json"
```

Use the actual inspected ID for `TARGET_INSTALLATION_ID`. A `normal` inspection
state is not an emptiness or source-isolation certificate. Preparation rejects a
source installation, mismatched identity or nonempty target.

Preparation writes and syncs the exact private target intent **before** changing
the database. That file includes a database password: never transport it with
the backup. Import takes its receipt pin and target identity from that intent.
Success is `committed_held`; a second import is refused. Historical credentials
and queued work are not reactivated. Use [current recovery access](RECOVERY-READING.md)
to grant reviewed read access to the imported results.

## If a command stops without reporting success

Do not delete the output, overwrite it or automatically repeat the operation.
After preparation or import, use the saved intent:

```sh
platform recovery target-inspect --target-config "$PRIVATE/target.json"
platform recovery import-inspect --target-config "$PRIVATE/target.json"
```

`empty_held` means the selected target is held without a committed import;
`committed_held` identifies its recorded import. A mismatch or failed inspection
requires investigation, not another import or removal of the hold. An existing
intent can also remain after preparation failed before changing the database.

After interrupted backup creation, keep the partial output for inspection. It
is not an acknowledged backup. Without a previously trusted receipt pin, do not
promote the adjacent receipt into an independently selected backup. A separately
authorized new backup must use a new directory.

File sync and process-crash handling do not prove physical power-loss recovery.
Reconciling events and costs after the snapshot, externally verifying source
isolation and enabling new work remain separate, unfinished recovery steps.
