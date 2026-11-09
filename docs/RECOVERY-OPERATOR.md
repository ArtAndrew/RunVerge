# Recover results from an encrypted backup

This development package supports recovery for reading. Use a separate target
database on PostgreSQL 18. The target stays closed to the normal API and workers.
Use synthetic data to evaluate the workflow. Work on a trusted maintenance host
with a local filesystem.

Set `PLATFORM` to the absolute path to the installed `bin/platform`, and
`PRIVATE` to an owned, private directory. Use mode 0700 for it and its staging
directory, and mode 0600 for configuration files. Keep configuration, keys,
tokens and scope files outside backups and source repositories.

Create `source.json` and `maintenance.json` with a single
`{"database_url":"EXPLICIT_PRIVATE_POSTGRESQL_URI"}` field. The maintenance
connection requires a PostgreSQL superuser on the target server and must have no
`search_path` override. Keep that connection for trusted maintenance commands.
Specify the host, port,
database, user, password and SSL mode explicitly. Use `sslmode=verify-full`
with an absolute `sslrootcert` for a remote connection, or
`sslmode=disable` for a deliberately local test. Do not pass the password on
the command line.

```sh
mkdir -m 700 "$PRIVATE/staging"
"$PLATFORM" recovery backup-keygen --out "$PRIVATE/archive-keys.json"
"$PLATFORM" recovery backup-create --database-config "$PRIVATE/source.json" \
  --keys-file "$PRIVATE/archive-keys.json" --staging-parent "$PRIVATE/staging" \
  --out-dir "$PRIVATE/backup-001"
```

Record the printed `receipt_sha256` independently of the backup. Set
`RECEIPT_SHA256` from that record. Keep a separate protected copy of the keys.
The backup directory and plan outputs must be new.

```sh
"$PLATFORM" recovery archive-verify --backup-dir "$PRIVATE/backup-001" \
  --keys-file "$PRIVATE/archive-keys.json" --staging-parent "$PRIVATE/staging" \
  --expected-receipt-sha256 "$RECEIPT_SHA256"
"$PLATFORM" recovery provision-plan --admin-config "$PRIVATE/maintenance.json" \
  --backup-dir "$PRIVATE/backup-001" --keys-file "$PRIVATE/archive-keys.json" \
  --staging-parent "$PRIVATE/staging" --expected-receipt-sha256 "$RECEIPT_SHA256" \
  --out "$PRIVATE/restore-plan.json"
```

Review the generated database name and set `TARGET_DATABASE` to that exact
name. Apply the plan within 15 minutes, without restarting the target primary.

```sh
"$PLATFORM" recovery provision-apply --intent-file "$PRIVATE/restore-plan.json" \
  --expected-database "$TARGET_DATABASE" --confirm-create-target
"$PLATFORM" recovery archive-import --backup-dir "$PRIVATE/backup-001" \
  --keys-file "$PRIVATE/archive-keys.json" --staging-parent "$PRIVATE/staging" \
  --target-config "$PRIVATE/restore-plan.json.target.json"
"$PLATFORM" recovery import-inspect --target-config "$PRIVATE/restore-plan.json.target.json"
```

The expected result is `committed_held`. After an interruption, use
`provision-inspect --intent-file ...` or `import-inspect --target-config ...`
with the saved plan or intent. Preserve those files, any one-use attempt marker,
and any incomplete output directory. Inspect the outcome before retrying,
deleting files or choosing a different target. A retry cannot overwrite an
existing backup, plan or import.

Prepare a private `scopes.json` using current access decisions reviewed outside
the backup. Its format is
`{"scopes":[{"subject":"reviewer","organization_id":"org_FROM_REVIEW"}]}`.
Set `ORG_ID` to the reviewed organization. Keep the issued token private.

```sh
"$PLATFORM" recovery access-init --authority-dir "$PRIVATE/authority" \
  --target-config "$PRIVATE/restore-plan.json.target.json" \
  --scope-file "$PRIVATE/scopes.json" --operator-reference "review:current-access" \
  --confirm-current-authority
"$PLATFORM" recovery issue-read-token --authority-dir "$PRIVATE/authority" \
  --subject reviewer --organization-id "$ORG_ID" --out "$PRIVATE/read.token"
"$PLATFORM" recovery list-tasks --authority-dir "$PRIVATE/authority" \
  --target-config "$PRIVATE/restore-plan.json.target.json" \
  --token-file "$PRIVATE/read.token" --organization-id "$ORG_ID"
```

Use `read-result` with the same four options as `list-tasks` plus
`--candidate-id` from the returned task. Results include the cutoff and hold
status. Costs are what was recorded at the cutoff. A read token expires after
five minutes by default. If the operator's current scope still permits access,
run `issue-read-token` again with a new private output filename and use that
file for later reads.

To back up the held target again, use `backup-create` with `--target-config`
pointing at its saved intent in place of `--database-config`, and choose a new
output directory. Record its receipt and restore into a new target with a new
plan. For that archive, pass the same `--backup-dir`, `--keys-file` and
`--staging-parent` used for import to both `import-inspect` and `access-init`
so they can verify the retained history.
