# Linux server preview

For a fresh installation with a local model and a ready team scenario, see the
[Compose team pilot](COMPOSE-PILOT.md). For HTTPS and corporate OIDC, use the
[organization Compose profile](ORGANIZATION-COMPOSE.md).

This source-build preview runs RunVerge's API, Workspace and worker on a Linux
server with Docker Compose. PostgreSQL runs in a separate container. The application
can use a separately configured model service; a model is not included in the image.

Use a fresh installation and test data only. This preview does not add production
backup/restore, supported upgrades, enterprise login or provider billing acceptance.
It is separate from the published macOS arm64 alpha.6 package. A successful image
build includes a native Linux installation/restart check; CI additionally checks the
Compose services together. Check the **Linux server preview** GitHub Actions result
for the exact revision you intend to try. An untested revision is not a supported
Linux release. Windows and named model providers on Linux need separate acceptance.

## Verified server architectures

The same source and Docker Compose configuration passed on native Linux **amd64
and arm64** hosts in [this verification run](https://github.com/ArtAndrew/RunVerge/actions/runs/40697158079)
at revision `e6bec0a944ce602d829497d43a4f5b83d614b417`. Each host built and installed
its own application binary, executed a synthetic task through a separate worker,
and retained the result and accounting records after restarting PostgreSQL, the API
and worker. The checks also installed the OpenRouter profile offline; no real model
was called. The hosts, Docker daemons, images and bundle identities were checked for
matching architecture. Both disposable test installations were removed afterward.

Docker selects the host architecture automatically; the setup commands are the same
on either server. Use a 64-bit Linux system. There is no prebuilt Linux download or
published container image yet: this preview builds from source. You can access the
server from a Windows, macOS or Linux computer using a browser and the SSH tunnel
below; that does not require a native RunVerge application on the client computer.

## Before starting

Use a Linux host with Docker Engine, Docker Compose v2, Python 3 and permission to
run Docker. Run commands from the repository root as your normal user. Allow space
for Go/Node build dependencies, PostgreSQL and image layers. The first build needs
internet access and takes longer than starting an existing image.

The server port binds to `127.0.0.1:8080`. PostgreSQL has no published host port.
The API uses the explicit `--container-preview` mode inside its container. Normal
`serve` commands still require a loopback listener. This mode supports the existing
platform-token access only; OIDC, HTTP MCP and trusted-proxy mode remain excluded.
Do not change the published port to `0.0.0.0` or expose it to an untrusted network.
For a remote server, use an SSH tunnel, for example
`ssh -L 8080:127.0.0.1:8080 your-server`, then open
[http://localhost:8080](http://localhost:8080) on your computer. Public HTTPS exposure
and a supported authentication deployment are outside this preview.

## Build and start a new installation

```sh
python3 scripts/server_setup.py
docker compose -f deploy/compose.yaml build api
docker compose -f deploy/compose.yaml up -d
```

The API allows 32 open task event streams per process by default. To change
that limit, set `RUNVERGE_EVENT_STREAM_LIMIT` in the Compose environment and
recreate the API service. For example, set it to `100` for a larger pilot.
Valid values are 1 through 128. Each person or service identity can open up
to four streams. Keep the chosen value in the deployment configuration for
future restarts.

The first command creates `deploy/private/` with separate random passwords for the
application and database administrator. It refuses to replace an existing directory. Do not commit,
share or paste these files, and do not print the fully resolved Compose configuration
into public reports. Keep this directory when restarting the same installation:
generating another password does not change a password inside an existing database.

For a fresh database, the setup creates `runverge_admin` for database administration
and `runverge` for the application. Only the database container receives the
administrator password in `db.env`. The API, worker, migration command and RunVerge
administration commands receive the application connection in `runtime.env`.
The application role owns its database, so it can install the schema, but it has no
PostgreSQL superuser, role-management, database-creation, replication or RLS-bypass
privileges. It cannot switch to the administrator role. Database ownership still
allows schema changes: this is not a restricted SQL account for untrusted code.

Readiness checks authenticate to PostgreSQL and verify the application role and a
marker linking the private settings to the database volume. Old settings, a volume
without the marker, or an elevated application role fail this check. Initialization
scripts run only for a new volume; they do not convert existing installations. Keep
an older installation on its matching source revision and settings until a supported
upgrade is available. Do not delete its volume, add a marker manually or replace its
password files to bypass a startup failure. An unhealthy status does not stop work
already running; these checks do not replace recovery isolation or access control.

The image contains the compiled application, Workspace, notices and a readiness
probe. Application services run as user `10001`, with a read-only root filesystem
and no Linux capabilities. These settings are a limited deployment profile;
they do not establish isolation for arbitrary untrusted agent code.

## Create the first owner once

Choose a new container name and output filename for this installation. The example
below assumes both names have never been used:

```sh
docker compose -f deploy/compose.yaml --profile admin run --no-deps --name runverge-first-owner admin bootstrap --name "My workspace" --out /state/first-owner.json
docker cp runverge-first-owner:/state/first-owner.json deploy/private/first-owner.json
chmod 600 deploy/private/first-owner.json
docker rm runverge-first-owner
```

The file contains owner credentials. Open it privately to sign in through the
Workspace; do not put its contents into a support ticket. If a command is interrupted,
inspect the named container and its output before attempting another bootstrap.
An uncertain result is not permission to create another organization blindly.

No real model is configured by these commands. The synthetic `fixture:echo` agent
can check the installation but does not generate an AI answer. Configure a model
explicitly before evaluating real tasks. See [local model configuration](LOCAL-MODELS.md)
and [the alpha quickstart](QUICKSTART-ALPHA.md). A model on the server's loopback
address is not reachable through a container's own `127.0.0.1`; do not copy the
macOS local endpoint unchanged. For the explicit OpenRouter text profile, see [OpenRouter setup](OPENROUTER.md).
The OpenRouter revision workflow was checked on macOS. Routine Linux CI uses
synthetic fixtures and installs the provider profile offline without a model key.
A separate alpha.6 Linux amd64 browser pilot used an owned loopback Qwen2.5:7b
server for the two-person team workflow; this does not verify OpenRouter on Linux.
See [alpha.6 validation](VALIDATION-alpha.6.md).

## Stop, resume and remove

```sh
docker compose -f deploy/compose.yaml stop
docker compose -f deploy/compose.yaml start
```

These commands keep the database volume and private settings. Do not delete the
volume to solve a startup problem. `down --volumes` permanently removes data and
is appropriate only when deliberately discarding a test installation. No supported
backup/restore or in-place upgrade procedure is provided in this preview.

## Verify without touching your installation

After building the image:

```sh
python3 scripts/verify_server_container.py --evidence /tmp/runverge-container-check.json
```

Choose a new evidence filename. The verifier creates its own randomly named Compose
project, private settings, database and owner, executes a synthetic task through the
separate worker, then restarts PostgreSQL/API/worker and compares the result and cost
records. It also checks separate database passwords, denied administrator/file/program
operations under the application login, and refusal of an elevated role or mismatched
volume settings. These checks use only the disposable database, including deliberate
temporary role and marker changes after stopping its application services.
It removes only that disposable project's containers and volumes. It does
not attach to the installation above, load its settings, call a paid model or prove
real-model quality. A failure retains private diagnostics locally; publish only the
sanitized evidence JSON.

## Dedicated installations for a provider trial

This local candidate provisions one Compose project per customer reference. Each
project has its own PostgreSQL volume, network and private database settings.
The API listens on a dedicated loopback port. Run this with test data on a
trusted Linux operator host. The operator controls the Docker daemon and the
host filesystem. Provider billing and the rest of the customer lifecycle require
separate acceptance.

Build the application and cache the pinned PostgreSQL image on that host first.
Use the Docker daemon on the standard Unix socket. Run the commands from the
repository root.

~~~sh
docker --host unix:///var/run/docker.sock build --target runtime -t runverge:provider-local .
docker --host unix:///var/run/docker.sock pull "$(awk '/^    image: postgres:/{print $2}' deploy/compose.yaml)"

provider_state="$HOME/runverge-provider-state"
mkdir -m 700 "$provider_state"  # once, outside the checkout
image_id=$(docker --host unix:///var/run/docker.sock image inspect \
  --format '{{.Id}}' runverge:provider-local)

python3 scripts/provider_provision.py --state-dir "$provider_state" \
  --tenant trial-a --name "Trial A" --port 18081 --image-id "$image_id"
~~~

The JSON reply gives the Compose project, the Workspace URL and the initial
organization and owner IDs. Run the same command after an interruption. Keep
the registry, the private database settings and the Compose volumes together.
Changing the name, budget, image ID or port for that reference is rejected.
To create a second test customer, use another reference and unused port.

Only the trusted operator can read the initial credential from the project's
private admin volume. Use the returned project name and the same image and port
as above. Copy the handoff to a new private directory.

~~~sh
project="runverge-tenant-..."  # copy the exact project name from the JSON reply
handoff_dir=$(mktemp -d "$provider_state/.handoff.XXXXXXXX")
handoff_container="$project-handoff-${handoff_dir##*.}"
(
  set -eu
  unset RUNVERGE_MODEL_ENV_FILE
  export DOCKER_CONFIG="$provider_state/.docker"
  export RUNVERGE_PRIVATE_DIR="$provider_state/trial-a/settings"
  export RUNVERGE_IMAGE="$image_id" RUNVERGE_PORT=18081
  docker --host unix:///var/run/docker.sock compose --env-file /dev/null \
    --project-name "$project" --file deploy/compose.yaml --profile admin \
    run --no-deps --pull never --name "$handoff_container" -T admin version
  trap 'docker --host unix:///var/run/docker.sock rm "$handoff_container" >/dev/null' EXIT
  docker --host unix:///var/run/docker.sock cp \
    "$handoff_container:/state/bootstrap.json" "$handoff_dir/bootstrap.json"
  chmod 600 "$handoff_dir/bootstrap.json"
)
~~~

Use the file privately to sign in at the returned URL. Treat the file and the
admin volume as credentials. Repeating provisioning preserves a prior revocation.

### Suspend or resume a test customer

Use the same private registry and customer reference. Read the current generation
before a change. A newly provisioned installation starts at generation 0.

~~~sh
python3 scripts/provider_provision.py --state-dir "$provider_state" \
  --tenant trial-a --operation status
python3 scripts/provider_provision.py --state-dir "$provider_state" \
  --tenant trial-a --operation suspend --expected-generation 0
python3 scripts/provider_provision.py --state-dir "$provider_state" \
  --tenant trial-a --operation resume --expected-generation 1
~~~

Suspension disables automatic restart and stops the selected API and worker.
Workspace is unavailable during suspension. PostgreSQL stays running and keeps
task history, results and costs. The transition leaves other installations
running. Resume starts the same API and worker. Queued work can continue when
its original approvals, deadline and budget still allow it. For a request
already sent to an external service, the normal outcome and accounting rules apply.

If a command is interrupted, inspect the recorded state and observed processes.
Repeat the exact operation with its original expected generation to finish a
transition in progress. A later change needs the newly reported generation.
Keep deployment control exclusive while these commands run. Resolve any changed
container, network or volume placement before retrying.

### Export a suspended test customer

Use a separate private key file for each customer. Keep the keys and archives
outside the checkout and provider registry, in separate private directories.
Run key generation once for the customer. The following example uses the same
pinned image and a customer suspended at generation 1.

~~~sh
keys_dir="$HOME/runverge-trial-a-keys"
archives_dir="$HOME/runverge-archives"
mkdir -m 700 "$keys_dir" "$archives_dir"
docker --host unix:///var/run/docker.sock run --rm --pull never --network none \
  --read-only --cap-drop ALL --user "$(id -u):$(id -g)" \
  --mount "type=bind,source=$keys_dir,target=/keys" "$image_id" \
  recovery backup-keygen --out /keys/archive-key.json

python3 scripts/provider_provision.py --state-dir "$provider_state" \
  --tenant trial-a --operation export --expected-generation 1 \
  --keys-file "$keys_dir/archive-key.json" --out-dir "$archives_dir/trial-a-001"
~~~

The export verifies the confirmed suspension and copies a consistent snapshot
from the dedicated database. The database must contain exactly the selected
organization. The output directory holds a signed, encrypted archive and its
receipt. Record the returned receipt digest separately. Keep the private key
separate from the archive and protect both during transport.

An interrupted export can leave a partial directory. Keep it for inspection.
The command requires a new output directory for each export. Use the key and
independently recorded receipt digest with archive verification in the
[recovery operator guide](RECOVERY-OPERATOR.md). Restoration starts on a
separate held database. Production key custody and the cloud provider's export
API require their own acceptance.

### Delete a suspended test customer

Run status to read the exact organization ID and current suspended generation.
This operation permanently removes that installation's containers, network,
database and admin volumes, and managed settings. Keep a verified export first
if you need its contents. Separate archive and key directories stay available.

~~~sh
python3 scripts/provider_provision.py --state-dir "$provider_state" \
  --tenant trial-a --operation status

organization_id="org_..."  # copy the exact ID shown by status
python3 scripts/provider_provision.py --state-dir "$provider_state" \
  --tenant trial-a --operation delete --expected-generation 1 \
  --confirm-organization-id "$organization_id"
~~~

Keep exclusive control of this Docker placement during deletion. The command
checks that the original database contains only this organization and records
its exact resources before removing them. Changed placement, unexpected
settings or a replacement volume require inspection.

If the command is interrupted, inspect status and repeat the original delete
with the same organization ID and expected generation. The final record
reserves the retired reference and allows its port to be used by another
customer.

The [local provider control API](PROVIDER-API.md) exposes these operations over
a private Unix socket and keeps a durable history for a panel integration.
