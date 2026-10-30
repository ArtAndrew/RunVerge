# RunVerge native bundle

Experimental 0.14.0-alpha.6 targets macOS arm64. Published packages are listed
on GitHub Releases. A separate Linux amd64/arm64 source/Compose preview is
described in docs/LINUX-SERVER.md. A service supervisor, automatic updates,
full backup/restore and release signing remain outside the supported alpha
profile.

The bundle contains a prebuilt Go server/worker/CLI, compiled Workspace, exact
schema identity, component inventory and dependency license notices. Running
it does not require Go, Node.js, Python, the source repository or the developer's
working-directory layout. PostgreSQL 18 is an explicit external prerequisite;
the development embedded PostgreSQL distribution is not bundled or installed.

## Inspect and install

Obtain a bundle from the trusted release channel and verify its published
checksum before executing any downloaded program. The first local package
does not include a publisher signature. Content hashes prove consistency
against the checksum you trust, not the identity of a publisher.

From the extracted bundle, use its `bin/platform` command:

```sh
./bin/platform version
./bin/platform bundle inspect --source /absolute/extracted-bundle --manifest-sha256 EXPECTED_HASH
./bin/platform bundle install --source /absolute/extracted-bundle --destination /absolute/new-installation --manifest-sha256 EXPECTED_HASH
/absolute/new-installation/bin/platform bundle verify --source /absolute/new-installation --manifest-sha256 EXPECTED_HASH
```

Use the SHA-256 of `bundle.json` from the build/release report as EXPECTED_HASH.
For development bundles, this is an explicitly trusted local build. Keep
the expected hash outside the installation. Paths in these examples are
placeholders; installation accepts only a new directory in an existing parent
that is not group/world-writable. Each artifact is bound to one OS, architecture,
API revision and ordered migration checksum list. These commands work without
DATABASE_URL and do not connect to a model, PostgreSQL or the network.

Installation never overwrites a directory, creates an organization, migrates a
database, installs a background service or starts execution. A completed
installation has an integrity-checked `installation.json` written last. A
failed/interrupted installation may leave an incomplete directory. Inspect and
remove that new directory deliberately before selecting a new destination;
the installer never deletes or reuses it automatically. Keep credentials,
database state, backups and logs outside the immutable installation tree.

## Explicit local startup

Configure DATABASE_URL privately for your separately managed PostgreSQL 18
database. Do not put a password in a command argument, repository, screenshot
or bug report. Then explicitly migrate that selected database and bootstrap a
new organization once:

```sh
/absolute/new-installation/bin/platform migrate
/absolute/new-installation/bin/platform bootstrap --name 'My organization' --out /absolute/private-state/bootstrap.json
/absolute/new-installation/bin/platform serve
```

Start the worker in a second terminal with the same private database environment:

```sh
/absolute/new-installation/bin/platform worker --concurrency 4
```

The default interface is http://127.0.0.1:8080. Its compiled assets are located
relative to the verified installed executable, independently of the current
directory. The listener retains the existing loopback restriction. A remote
multi-user installation needs the separately reviewed HTTPS/proxy and identity
configuration; do not expose a development listener as a public service.

Bootstrap creates an organization and its owner, not an agent. Create and
publish an agent in Workspace after connecting with the organization ID and
token from the private bootstrap file. The agent form can suggest an installed
local model with an explicit zero API tariff. Check the selected connection;
choose a paid model explicitly. The deterministic `fixture:echo` executor
only repeats test input; select a real model for useful AI work.
Model/API credentials remain outside the bundle. Token-based estimates are
not final billed amounts; unresolved costs retain their reservations. A
successful model response remains subject to human review.

API0.14 additionally supports an explicit operator-declared local zero API
tariff. The bundled `LOCAL-MODELS.md` describes the private profile, installation,
request limits and uncertainty count. It does not claim free hardware or enforce
physical CPU/GPU quotas. Deploy matching API/UI/SDK revisions; the saved older
main installation is not upgraded by a package build or these instructions.

## Updates and recovery

This first installer deliberately creates a separate application directory.
It does not yet implement an upgrade or downgrade protocol, verify a live
server/worker version matrix, or prove restoration from a database backup.
Do not point a new installation at a production database as an upgrade.
The bundled migration inventory is a description of embedded inputs, not
evidence that a database has applied them. Restore must hold external execution
until credentials, prior revocations and possible external effects have been
reconciled; copying a database backwards in time is not a safe retry mechanism.

## Rebuild from source

With the documented developer dependencies already installed, run:

```sh
python3 scripts/build_native_bundle.py --output /absolute/new-bundle --version 0.14.0-alpha.2
```

The output directory must be new and outside the checkout. Builds use pinned
local dependencies with network dependency downloads disabled. Only the
binary, compiled web, source hashes, instructions, inventory and license
notices enter the bundle. Private settings, fixtures, working data and test
evidence are excluded. This is a component inventory, not yet a standard
SPDX/CycloneDX SBOM or signed supply-chain provenance.

## Repeat work in Workspace

On a completed top-level task, choose **Use for a new task**, replace the source
text, check the currently published agent and limits, then confirm creation.
This starts a separate task; the original results, feedback and accounting stay
unchanged. Opening the form does not call a model.

Use **Compare results** to read two saved versions and their exact feedback.
The view verifies artifact identity and content digests; it does not check facts,
accept a result or run a model. The panels stack on narrow screens.

## Team walkthrough

Bundles also include [TEAM-REVIEW.md](TEAM-REVIEW.md) and
[TWO-PERSON-PILOT.md](TWO-PERSON-PILOT.md). Start with two published agents,
then use separate owner and member credentials on a fresh local installation.
These guides describe a test-data
walkthrough, not a supported remote team service or enforced independent approval.
