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
The helper uses the running process's database pool, not a substitute connection.
The observer in the pinned database container matches fresh challenges and peer
addresses against the planned API and worker endpoints. All helpers must finish
before the shutdown plan can be saved or used.

This profile requires direct `db:5432/runverge` connections, the restricted
`runverge` role, one Compose bridge network, and two to eight API/worker processes.
Proxy databases, extra peers, alternate DNS, active auxiliary containers and
unrelated exec processes are refused. Stopped admin/migrate containers must have
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
restarted identities. An installed Compose check is included for live binding,
restart refusal and fresh-process stop inspection; this candidate is not accepted
as a supported restore path until that check and the remaining recovery steps pass.
