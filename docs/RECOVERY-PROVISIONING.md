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
backup directory and any source repository. The selected backup must use the same
trusted schema as this binary. This does not convert an alpha.6/SQL022 backup.

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

[The disposable PostgreSQL run](https://github.com/ArtAndrew/RunVerge/actions/runs/41316164535)
passed creation, authenticated import, exact result reading with fresh access,
revocation and retained execution hold. It also checked raw connection denial for
restricted application credentials, isolation checks on reconnect, and wire loss
before and after the database creation statement. Commands ran through compiled
test helper processes. This is not an installed-package restore drill, a test of a
specific old application binary, a macOS deployment check or permission to enable
new work. The later source change only corrects this test's explanatory comment.
