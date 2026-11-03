# Stop a source Compose project — development candidate

These host commands stop every container in a reviewed local Docker Compose
project and disable its automatic restart. They are not included in alpha.6.
Evaluate them only on disposable installations. They leave database volumes intact
and never start a container, delete data, revoke an external credential or release
the restored target's hold.

This is a process-shutdown component, not a complete recovery procedure. A project
name or Docker label does not prove which RunVerge installation owns the processes.
The operator must identify the source independently and review every container in
the plan. The separate `source-stop-*` candidate below adds live source binding. Target connection
isolation is covered by the provisioning component; external credentials,
post-backup reconciliation and complete recovery remain unfinished.

## Boundary and prerequisites

Run the command on the Linux Docker host using an explicit Unix socket. Docker
Engine API 1.45 must be supported. No `DOCKER_HOST`, Docker context, proxy or remote
TCP fallback is used. The host, Docker daemon, images, protected socket and its
ancestors are trusted. Only the operator may change containers during this operation;
disable deployment automation and prevent other processes from controlling Docker.
Source processes must have no Docker API access, including unauthenticated TCP,
and no execution outside the reviewed host. This command does not establish those
host-wide prerequisites or defend against a compromised host/kernel.

The initial profile requires one `db`, one `api`, at least one `worker`, and only
optional `migrate`/`admin` containers, with at most 32 containers total. It refuses
Swarm, live-restore, paused/restarting/dead containers, elevated device access,
shared host namespaces, automatically removed containers and unreviewed mounts.
Application containers must retain the preview's non-root, read-only configuration.
Database and admin volumes must use the local driver without extra mount options.

**The database container is stopped too.** Use a dedicated source project and
database server. Do not run this against a shared PostgreSQL container that also
hosts the restored target or other applications. The earlier
[database-admission component](RECOVERY-DATABASE-FENCE.md) uses distinct databases
on one primary; that alone does not satisfy this separate-project requirement.
Export and verify the backup before stopping its database server.

## Plan, review and apply

Use a private directory owned by your OS account with mode `0700` and protected
ancestors/ACLs. The backup directory must exist; the new plan must be outside it.
All paths are explicit. Replace `SOURCE_PROJECT` with the independently identified
source project:

```sh
platform recovery container-stop-plan \
  --docker-socket /var/run/docker.sock --project "$SOURCE_PROJECT" \
  --backup-dir "$PRIVATE/backup-001" --out "$PRIVATE/container-stop.json"
```

Planning captures the daemon identity and full container IDs, names, creation times,
image identities, services, process start times and restart counts. It performs no container mutation. Review that exact
inventory before applying within 15 minutes:

```sh
platform recovery container-stop-apply \
  --intent-file "$PRIVATE/container-stop.json" \
  --expected-project "$SOURCE_PROJECT" --confirm-stop-project
```

Before mutation, the command creates a durable, private `.apply` attempt marker next
to the plan. It rechecks the complete inventory, disables automatic restart, confirms
the policy changes, then force-stops application containers and finally PostgreSQL.
This is a recovery stop with no task-draining grace period. It uses
[Docker's versioned update and stop API](https://docs.docker.com/reference/api/engine/version/v1.45/#tag/Container/operation/ContainerStop).
The completion report is based on fresh container and inventory observations.

## Interrupted or uncertain operation

A lost acknowledgment or output error is not a successful stop. Inspect using the
saved plan in a new process:

```sh
platform recovery container-stop-inspect --intent-file "$PRIVATE/container-stop.json"
```

`project_processes_stopped: true` means the exact bound inventory was observed
stopped, without live PIDs or exec instances and with automatic restart disabled.
It is not a permanent certificate: an operator can still start or recreate a
container. A later manual start invalidates the saved process identity; replacement, removal,
extra containers or a changed daemon identity refuse the observation.

The unbound `container-stop-*` commands always report `source_installation_bound: false`,
`external_execution_fenced: false` and `external_effects_settled: false`.
Requests already delivered to a remote service may finish or incur costs after the
local process stops. Do not release reservations or resend historical work based
on this report or its timestamp.

A second apply with the same plan is refused by the attempt marker. If inspection
shows only a partial stop, investigate the cause and create a new plan for the
remaining reviewed inventory; a new explicit confirmation is required. Do not
delete the marker or turn this into an automatic retry loop. An expired plan can
still be inspected but cannot authorize new mutations.

The Linux Compose verifier checks this path on its own disposable project. A
synthetic sender issues requests without a database connection, then stops with the
project. The check loses command output, inspects from a new process, refuses a
second apply and detects a deliberately restarted sender. Its short observation
window does not prove settlement of external work or full restoration.

## Bind shutdown to a selected backup and live installation

The new `source-stop-*` commands require this candidate's Compose API and workers,
with the explicit `RUNVERGE_RECOVERY_CONTROL=container_v1` setting. Each PID1 runtime
serves a read-only witness on a private Unix socket; it is not an HTTP API feature.
Compose pins the temporary filesystem owner/group to `10001` and mode to `0700`,
including after restart. Do not substitute a one-time chmod: [Docker documents
permission resets for tmpfs mounts](https://docs.docker.com/engine/storage/tmpfs/).
The helper uses the running process's database pool, not a substitute connection.
The observer in the pinned database container matches fresh challenges and peer
addresses against the planned API and worker endpoints. All helpers must finish
before the shutdown plan can be saved or used.

This profile requires direct `db:5432/runverge` connections, the restricted
`runverge` role, one Compose bridge network, and two to eight API/worker processes.
Proxy databases, extra peers, alternate DNS, active auxiliary containers and
unrelated exec processes are refused. A read-only observation may wait up to two
seconds for a short healthcheck or completed helper to disappear; it never ignores
an exec that remains present or restarts a command to make the check pass. Stopped admin/migrate containers must have
restart disabled. The operator must supply the runtime image ID from an independently
trusted build/deployment record; do not copy it blindly from an untrusted plan.
The host/Docker/exclusive-operator prerequisites above still apply.

```sh
platform recovery source-stop-plan \
  --docker-socket /var/run/docker.sock --project "$SOURCE_PROJECT" \
  --backup-dir "$PRIVATE/backup-001" --keys-file "$PRIVATE/archive-keys.json" \
  --staging-parent "$PRIVATE/staging" \
  --expected-receipt-sha256 "$RECORDED_RECEIPT_SHA256" \
  --expected-runtime-image "$TRUSTED_RUNTIME_IMAGE_ID" \
  --out "$PRIVATE/source-stop.json"

platform recovery source-stop-apply \
  --intent-file "$PRIVATE/source-stop.json" \
  --expected-project "$SOURCE_PROJECT" --confirm-stop-project

platform recovery source-stop-inspect --intent-file "$PRIVATE/source-stop.json"
```

Planning authenticates the selected archive and validates its snapshot framing and
schema digest. It saves the snapshot digest, source installation ID, observed
PostgreSQL system/database identity, exact inventory and live process boot IDs.
Apply repeats the live witness before recording its one-use attempt and stopping
containers. Restarting even the same container invalidates the old plan. Bound and
unbound command families cannot be mixed to bypass this check.

A successful bound inspection reports `source_installation_bound: true` and, only
when every selected process is stopped, `bound_source_processes_stopped: true`.
It still reports `external_execution_fenced: false` and
`external_effects_settled: false`: this does not locate every possible executor on
other hosts, settle already dispatched requests, or authorize new work on a
restored target. The archive cutoff and stop time are different events.

Local protocol/profile tests cover malformed requests, cancellation, concurrent
witness refusal, changed endpoints, foreign peers, indirect connections and
restarted identities. The installed Compose verifier passed on native Linux amd64
and arm64 at commit `f7cb1bc7d2c39069c38ce750e9591bf2716dcb6d` in
[GitHub Actions run 41323599315](https://github.com/ArtAndrew/RunVerge/actions/runs/41323599315).
Both architectures used source digest
`7beaf1e90c8727c4f5ed2edbea1805ff68bc6464762772b4d1f1d9dc6f09cabe`.

That check created an authenticated backup using the installed operator CLI,
obtained witnesses from the live API and worker, refused a persistent unrelated
exec, invalidated a plan after an API restart, refused a wrong source ID and a
command-family bypass, then lost shutdown output and inspected from a fresh
process. Repeated apply was refused. Restarting the database retained its source
installation identity. The existing task/result/cost restart check and synthetic
sender shutdown also passed. Disposable resources were removed; no real model or
working installation was used.

This accepts the source-binding/process-shutdown component within its declared
Linux profile. It does not accept complete restoration, external-effect settlement,
post-backup accounting reconciliation, enabling new work, a populated alpha.6
upgrade or native macOS source shutdown. Published alpha.6 is unchanged.
# Post-backup source evidence (development candidate)

The source-stop plan accepts an optional `--evidence-dir /private/new-evidence`
on a native Linux Docker host. This directory must be new and separate from the
selected backup, keys, staging directory and saved intent. The host must reach
its own Docker bridge; this does not publish a database port or add a Docker
Desktop/macOS capture profile. Keep exclusive operator control throughout.

Before shutdown, apply reauthenticates the selected archive and checks the direct
database connection. It then disables container restart, stops application
executors and waits for other database client sessions to finish. Prepared
transactions prevent capture. The evidence snapshot starts only after that
barrier. A fresh nonce, observed inside the exact database container, binds the
export transaction to the source. The export uses a read-only transaction and
performs no migrations or charge resolution. Its operator connection still
uses a trusted database administrator.

The new encrypted archive preserves the observed business rows, including
post-backup facts. A separately signed `.capture` file binds its digest to the
selected older archive and source plan; `.capture-state` preserves the observed
executor state covered by that signature. These private files contain recovery
metadata, not credentials. Keep them with the private intent, outside both
archives. The signed link attests evidence capture, not final database shutdown.
Only after saving that link does apply stop PostgreSQL.

`source-stop-inspect` rechecks the signed link and encrypted archive and reports
`evidence_captured`. It always reports `evidence_reconciled: false`. The candidate
[evidence ingestion commands](RECOVERY-EVIDENCE.md) preserve the late snapshot
inside a held target; financial reconciliation remains unfinished. Neither
capture nor local shutdown proves that external requests have settled. An
uncertain request remains uncertain; historical work must not be resent.

Failure can leave executors stopped and the database running. Inspect the saved
plan and outputs; do not repeat apply, overwrite partial evidence, or restart
agents automatically. A lost final output can be inspected in a fresh process.
This candidate is not a supported complete restore flow or an alpha.7 release.

Checked implementation: `d325547ca50e236dd0d5e72d2bb192359e26bd9e`.
[PostgreSQL verification](https://github.com/ArtAndrew/RunVerge/actions/runs/41331278044)
passed source identity/cutoff/witness refusals, outstanding client and prepared
transaction barriers, a late committed row in the exported snapshot, and
signature splice/wrong-key refusal. The concurrent drain test includes a timing
wait; it does not deterministically control a server-side COMMIT crash boundary.
[Native Linux verification](https://github.com/ArtAndrew/RunVerge/actions/runs/41331283620)
passed on amd64 and arm64: the installed source-stop command captured evidence,
lost its final output, then a fresh inspection authenticated the archive and
link and confirmed the source containers stopped. Both disposable environments
were cleaned up; no real model was called. These checks do not yet import the
late evidence into a restored target or prove external billing reconciliation.

## Signed final stop observation — development candidate

When evidence capture is configured, a successful `source-stop-apply` now saves
`INTENT.source-stop` after PostgreSQL has stopped. This separate signed file binds
the final observed container state to the exact `.capture` link, both archive
digests, original source binding and Docker process generations. The earlier
`.capture` and `.capture-state` files still describe capture before database stop;
their format and meaning are unchanged.

Inspect the saved final observation and recheck the original Docker inventory:

```sh
platform recovery source-stop-inspect \
  --intent-file "$PRIVATE/source-stop.json" \
  --source-stop-file "$PRIVATE/source-stop.json.source-stop"
```

The command refuses changed container generations, missing or additional
containers, a changed daemon, a running source or enabled automatic restart. A
restart followed by another stop also invalidates the original process identity.
Signature verification alone proves historical authenticity; the command also
performs a fresh read-only observation. It does not restart or stop anything.

If the final receipt was not saved completely, keep it for inspection and write
a fresh observation to a **new** private path:

```sh
platform recovery source-stop-inspect \
  --intent-file "$PRIVATE/source-stop.json" \
  --source-stop-out "$PRIVATE/source-stop-observed-again.json"
```

The original plan may be older than the 15-minute destructive-apply limit,
provided every original process identity still matches. The output must be
outside both archives; existing files are never overwritten. Ordinary inspection
without either flag continues to report source state and capture without writing
a receipt. A lost stdout does not permit repeating apply.

This is a point-in-time observation under exclusive control of the trusted local
Docker host. Keep that control throughout recovery: the receipt cannot prevent a
later manual restart or execution elsewhere. The database identity comes from the
earlier live binding; a stopped database does not provide a new SQL witness.
`window_covered`, `new_work_allowed`, `external_execution_fenced` and
`external_effects_settled` remain false. This component does not reconcile costs,
clear the restored target, enable new tasks or establish complete recovery.
