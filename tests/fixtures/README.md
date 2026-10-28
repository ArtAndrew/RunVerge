# Explicit local synthetic peers

These peers perform no paid inference or real business action. They are opt-in
test infrastructure, not part of normal startup or a production provider.
After the local bootstrap described in [DEVELOPMENT.md](../../docs/DEVELOPMENT.md),
run from the repository root in separate terminals:

```sh
python3 scripts/dev.py fixture action
python3 scripts/dev.py fixture read
python3 scripts/dev.py fixture model
```

Wait for the action peer's readiness message before starting the read peer:
they share the local test certificate. Leave the required processes running
during live tests. These fixture commands require Python on a POSIX host, with
`openssl` for the first TLS certificate; they do not require Go or PostgreSQL
merely to start the peers. No system CA or service is installed.

The commands find the repository from the location of `scripts/dev.py`, so an
absolute script path also works from an unrelated working directory. The native
peer additionally supports a direct, explicit repository argument:

```sh
python3 /absolute/repo/tests/fixtures/native_model_peer.py --repo /absolute/repo
```

All peers bind loopback only, reuse their saved port on restart, and write
private profiles, random credentials and SQLite journals under ignored `.local/`
with 0600 files and a 0700 directory. The native peer prints only readiness,
port and model name; importing its module does not start a server, parse command
arguments or create fixture state.

Install the explicit synthetic profiles through the trusted local CLI:

```sh
python3 scripts/dev.py install-tool --file .local/tool-fixture-profile.json
python3 scripts/dev.py install-tool --file .local/read-fixture-profile.json
python3 scripts/dev.py install-model --file .local/native-model-profile.json
```

Start one development worker with explicit fixture credentials:

```sh
python3 scripts/dev.py worker --fixture-credentials --concurrency 4
```

If a worker is already running, stop that worker before replacing it. This
opt-in loader reads only `tool-fixture-secrets.json`, `read-fixture-secrets.json`
and `model-fixture-secrets.json`, checks their exact environment references and
passes values to the child process in memory. It rejects symlinks, nonregular or
oversized files, exposed permissions, duplicate JSON fields and conflicting
inherited keys. No file is evaluated as a shell script. Without the flag, keys
are not loaded automatically. Never paste values into command arguments, logs,
reports or Git. This development helper does not replace a production secrets
broker.

The action peer has success, reject and deliberate response-loss modes. Its
SQLite journal distinguishes physical requests from synthetic effects. The
model peer returns a native call using the supplied pinned alias, then a final
unverified response after a quoted tool observation. A synthetic user goal
containing `unknown` selects response-loss mode. Stable request IDs replay the
fixture's own stored response; that property is not attributed to arbitrary
real providers.

The native peer keeps the existing v1 model profile values and filenames:
`native-model-profile.json`, `model-fixture-secrets.json` and
`native-model-fixture-ledger.sqlite3`. Restart preserves the credential, port,
profile bytes and stored responses, including after abrupt process termination.
Restore that complete private fixture state together and keep the bootstrap
organization unchanged. A missing ledger or credential, incompatible profile,
or unavailable saved port fails startup rather than replacing installed pins.
An OS advisory lock prevents a second native peer from rewriting a running
peer's state; a process crash releases the lock automatically. The lock file
may remain and does not need manual deletion.

With API, worker, built TypeScript SDK and all three installed peers running,
the live SDK runner uses the public synthetic manifest
`tool-scenarios.json` by default:

```sh
python3 tests/sdk/run_platform_e2e.py --report .local/qa/sdk-report.json
```

It checks the required installed catalog before creating resources, then runs
20 Python/TypeScript flows using `fixture:echo` and the explicit peers. It
creates persistent synthetic tasks and reservations. Use a new report filename
for each run to retain prior evidence.

Restoring a peer does not authorize resending an unknown occurrence. Keep that
occurrence stopped and use a newly authorized test task for an independent run.
The failed live infrastructure baseline is retained separately from passing
runs. Source fingerprints in `docs/evidence/model-loop-peer-sources-0.7.json`
describe the initial captured sources, before the later portable native-peer
changes. Those historical reports are not fingerprints of today's source. Go
integration tests also construct their own isolated local peers.

Run the portable native-peer regression suite without the platform or database:

```sh
python3 -m unittest discover -s tests/fixtures -p test_native_model_peer.py -v
```

These tests launch real subprocesses from an unrelated working directory in
temporary fake repositories. They exercise HTTP text/proposal/final responses,
exact integers above 2^53, authentication and bounded-input rejection,
concurrent replay/conflicts, hard restart with a persisted cache, preservation
of existing profile bytes and a second-process lock. The author execution was
on macOS; Windows execution and cross-platform acceptance are not established.
This is fixture reproducibility, not production database backup/restore or
acceptance of a real provider's billing and retry behavior.

The 0.8 read peer performs an immutable lookup of 42 and records requests only.
It does not create business-effect records; its declared ceiling9 is synthetic.
Its read-only opt-in is part of the installed trusted profile. Do not relabel the
write fixture as a read tool to bypass automatic-approval policy.

The initial 0.8 UI phase reports record the state before the subsequent
server-only revocation cleanup. Those historical waiting-approval assertions
are not current cleanup regressions. The current executable UI scripts assert
the cleanup behavior. The final fresh 0.8 phase and independent database marker
observation are recorded in
`docs/evidence/automation-cleanup-live-0.8/` and
`docs/evidence/automation-cleanup-live-sql-0.8.json`; executable Go regression
coverage is in `internal/storage/automation_*test.go`.
