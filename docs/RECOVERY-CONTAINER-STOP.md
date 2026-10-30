# Stop a source Compose project — development candidate

These host commands stop every container in a reviewed local Docker Compose
project and disable its automatic restart. They are not included in alpha.5.
Evaluate them only on disposable installations. They leave database volumes intact
and never start a container, delete data, revoke an external credential or release
the restored target's hold.

This is a process-shutdown component, not a complete recovery procedure. A project
name or Docker label does not prove which RunVerge installation owns the processes.
The operator must identify the source independently and review every container in
the plan. Verified source/target identity binding, target connection isolation,
external credentials and post-backup reconciliation are still unfinished.

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
image identities and services. It performs no container mutation. Review that exact
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
container. A later manual start is reported as not stopped; replacement, removal,
extra containers or a changed daemon identity refuse the observation.

The command always reports `source_installation_bound: false`,
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
