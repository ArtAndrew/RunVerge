# Linux server preview

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

The first command creates `deploy/private/` with a random database password and
private settings. It refuses to replace an existing directory. Do not commit,
share or paste these files, and do not print the fully resolved Compose configuration
into public reports. Keep this directory when restarting the same installation:
generating another password does not change a password inside an existing database.

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
records. It removes only that disposable project's containers and volumes. It does
not attach to the installation above, load its settings, call a paid model or prove
real-model quality. A failure retains private diagnostics locally; publish only the
sanitized evidence JSON.
