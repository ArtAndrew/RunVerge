# Disconnect the source database — development candidate

This recovery component stops new connections to a selected source database and
terminates its existing database sessions. It is not in alpha.6. Use disposable
test installations while evaluating it. The restored target remains held.

**A disconnected database does not prove that all old agents stopped.** An agent
may already hold a dispatch permit in memory and send an external request after
its database session ends. These commands always report
`external_execution_fenced: false`. Their observation time must not be used as
the end of the period needing external-effect and cost reconciliation.

## Initial deployment profile

Use one trusted PostgreSQL 18 primary with three distinct databases:

- the source application database, whose connections will be stopped;
- a separately provisioned, held recovery target;
- a maintenance database used by the recovery operator.

Different schemas in one database are insufficient. The command disconnects
**every session in the source database**, across all schemas. Use a dedicated
application database. It does not change data rows, resolve prepared transactions,
enable the target, delete a database or provide a command to reopen the source.

The maintenance connection needs a trusted superuser in this initial profile,
so it can observe and terminate every source session. Keep that credential out
of application configuration and out of archives. The database administrator,
operating-system owner and authenticated database endpoints are trusted. A DBA
can undo a connection ban. Clones, replicas, failover, arbitrary background
extensions and distributed executor shutdown are outside this profile.

Use the private directory and explicit connection rules from
[archive recovery](RECOVERY-ARCHIVES.md). Prepare private 0600 `source.json` and
`maintenance.json` files containing `{"database_url":"EXPLICIT_PRIVATE_URI"}`.
Use the existing private `target.json` created when preparing the held target.
Prefer verified TLS for remote endpoints; do not send passwords over untrusted
networks. The local filesystem and private ancestor/ACL requirements still apply.

## Review the exact database before stopping it

```sh
platform recovery database-fence-plan \
  --source-config "$PRIVATE/source.json" \
  --target-config "$PRIVATE/target.json" \
  --admin-config "$PRIVATE/maintenance.json" \
  --backup-dir "$PRIVATE/backup-001" --out "$PRIVATE/database-fence.json"
```

Planning reads the trusted source/target catalog and installation identities.
It also makes the maintenance connection observe a fresh nonce on each live
source/target database connection. Equal server names or control-file IDs alone
are not accepted as a shared-primary witness. Source, target and maintenance
database identities must differ.

The output contains a reviewable source database name and operation identity.
New plans use version 2 and also pin the current primary's WAL timeline, the
selected backup and the held target's restore and installation identities.
The new private intent file also contains the maintenance connection password;
it cannot be created inside the supplied backup directory, including aliases.
All three current configuration inputs must also come from outside that backup
directory; symlinked or hardlinked privileged files are refused. Existing output
is never overwritten. No database is disconnected by planning.

Review the whole-database impact and set `SOURCE_DATABASE` to the exact name in
that plan. Apply within 15 minutes:

```sh
platform recovery database-fence-apply \
  --intent-file "$PRIVATE/database-fence.json" \
  --expected-source-database "$SOURCE_DATABASE" --confirm-disconnect-source

platform recovery database-fence-inspect \
  --intent-file "$PRIVATE/database-fence.json"
```

The command takes a short PostgreSQL catalog lock that serializes database DDL
across maintenance connections. It verifies the bound source and target names
and IDs, then commits the connection ban and the version 2 receipt together.
The receipt resides in the source's database-wide catalog settings. It binds the
complete plan and original transaction ID and contains no credentials. A second
plan for the same source cannot adopt this receipt. Only a confirmed commit
permits automatic session termination. Each signal checks the receipt, process
ID and backend start time together.

PostgreSQL's [connection-admission setting](https://www.postgresql.org/docs/18/sql-alterdatabase.html)
and [bounded backend termination](https://www.postgresql.org/docs/18/functions-admin.html#FUNCTIONS-ADMIN-SIGNAL)
provide the underlying operations. The command then refreshes its observation
rather than treating delivery of a signal as proof of completion.

## Inspect an interrupted operation

If output or a database acknowledgment is lost, inspect using the saved intent
and a fresh maintenance connection. Do not repeat apply, delete the intent or
reopen the source automatically.

| Observed state | Meaning |
|---|---|
| `connections_allowed` | This observation has not established a connection ban. Investigate before another apply. |
| `connections_disabled_unresolved` | New connections are disabled, but sessions or prepared transactions remain. |
| `database_admission_fenced` | New connections are disabled and this observation found no sessions or prepared transactions. External execution is still unverified. |

For a committed version 2 plan, `receipt_sha256` identifies the stored receipt.
Keep using the original intent. A fresh inspection can verify the same receipt
after a restart of the same primary. In that case the result includes
`postmaster_changed: true`. A plan whose ban was never committed requires a new
live review after restart. The apply command always requires the original server
start identity.

After reviewing an interrupted disable, an operator can explicitly finish
terminating remaining sessions without repeating the admission change:

```sh
platform recovery database-fence-drain \
  --intent-file "$PRIVATE/database-fence.json" \
  --expected-source-database "$SOURCE_DATABASE" --confirm-disconnect-source
```

Drain has a bounded wait and refuses an enabled source. It never commits or
rolls back a prepared transaction. Such transactions remain visible as unresolved
evidence requiring separate investigation. More than 256 observed sessions is
outside this command profile and returns no successful fence observation.

Use a direct, trusted maintenance endpoint for the same physical primary. A
changed timeline, missing receipt on a disabled version 2 source, changed source
catalog row or changed database-wide settings row invalidates the proof. Keep
the disconnected database and its settings intact. Once the original transaction
is outside PostgreSQL's unambiguous transaction ID range, investigate under a
new operator review. Version 1 intents retain their original same-start
behavior for sources without a version 2 receipt.

This observation covers admission to the specified database on that primary.
An identical physical copy can share the database identity and receipt. Verify
executor and credential shutdown, reconcile the period since the snapshot and
establish current target authority before allowing new tasks.
