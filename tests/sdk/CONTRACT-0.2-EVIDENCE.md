# SDK contract 0.2 verification

Date: 2026-10-23. Base checkpoint: `05b273b`, plus the uncommitted SDK extension
and the integrator's contract 0.2 changes. No package was published.

## Implemented boundary

Both SDKs now expose read-only task-scoped `runs`, `operations`, `actions`,
`approvals`, and `costs` methods. Four execution collections return ordinary
pages with opaque cursors. Costs return a report with three independently
paginated collections and authoritative subtree totals. Inputs map to the
integrator-confirmed routes and query names. Both package versions are 0.2.0.

There are no billing ingestion, settlement, refund, hold-release, or approval
decision methods. Existing 0.1 methods retain their signatures. The additional
Task fields are optional in SDK types to retain compatibility with old server
responses, while documented as present on 0.2 servers (except root parent_id).

## Automated HTTP checks

The following passed on Python 3.12.14 and Node.js 24.19.0:

```sh
python3 -m unittest discover -s tests/sdk -v
```

Result: **16 passed**. Use Python 3.10 or newer; the execution used the bundled
Python 3.12.14 executable. Temporary test HTTP servers bind to loopback only.

From `packages/typescript-sdk`:

```sh
pnpm test
```

Result: **22 passed**, SDK build and compile-only public consumer declarations
passed. `tests/public-types.ts` imports the built public declarations and checks
both the new exports and a 0.1 Task object without the added fields.

New checks cover all five routes, safe task ID encoding, authentication and
organization scope headers, GET-only requests without mutation keys, opaque
cursor preservation, independent cost cursors, signed ledger deltas, synthetic
labels, unknown totals and financial holds, malformed report rejection, and
404 preservation without fallback or retry. Existing cancellation, timeout,
redirect, error, and mutation-key tests continue passing. The shared
`execution-fixtures.json` is HTTP test data, not evidence of actual execution.

## Packaging checks

Python wheel built with setuptools 84.0.0 on Python 3.12.14. The resulting
`agent_platform_sdk-0.2.0-py3-none-any.whl` includes `py.typed`, license and all
SDK modules. Metadata has version 0.2.0 and no third-party runtime dependencies.
After extracting the wheel into a temporary location, an isolated Python
process (`-I`) imported all new public types and verified the five methods.

Equivalent build command from `packages/python-sdk`, with a supported Python
and the declared build backend installed:

```sh
python3 -c 'from setuptools.build_meta import build_wheel; build_wheel("/tmp/agent-platform-sdk-wheel")'
```

The first attempted build used macOS Python 3.9 and its old setuptools, which
does not satisfy this package's requirements. It produced invalid UNKNOWN
metadata; it was rejected and cleaned up. The subsequent supported-runtime
build above passed. The existing `project.license` TOML-table syntax produces
a setuptools deprecation warning; the wheel metadata is correct.

TypeScript packed with the bundled pnpm CLI, which ran `prepack` and generated
`agent-platform-sdk-0.2.0.tgz`. The tarball includes built JS, declarations,
source maps, source, README and Apache-2.0 license; it contains no credentials.
It was extracted into an isolated temporary `node_modules/@agent-platform/sdk`
location. Runtime imports, all five methods, package metadata, and a strict
TypeScript consumer compiling all new exported types passed.

Equivalent command from `packages/typescript-sdk`:

```sh
pnpm pack --pack-destination /tmp/agent-platform-sdk-02
```

No runtime dependency was added. `git diff --check` passed for both SDKs and
SDK tests. These package checks verify locally built artifacts; no registry
upload or installation from a public registry was performed.

## Live scenario

`run_platform_e2e.py` now exercises both SDK examples and, for each resulting
task, checks execution and accounting with **both** SDKs. It traverses real
cursor pages with `limit=1`, rejects duplicates/repeated cursors, compares
collection counts, and checks a settled, explicitly synthetic fixture charge.
The TypeScript reader is `read_execution.ts`. Neither writes accounting data.

Run from the repository root after building the SDK and starting the updated
server and fixture worker:

```sh
python3 tests/sdk/run_platform_e2e.py --node node
```

The runner reads `.local/bootstrap.json` in memory, passes credentials through
child-process environments, and redacts the token from captured failures. It
creates one explicitly labeled fixture task per SDK; costs are conditional
test amounts, not provider bills.

### Executed against API 0.2

On 2026-10-23, after the integrator applied migration 003 and restarted the
local API and worker, the full scenario completed with **exit code 0** against
`http://127.0.0.1:8080`. Exact executed command from the repository root:

```sh
/workspace/.cache/codex-runtimes/codex-primary-runtime/dependencies/python/bin/python3 tests/sdk/run_platform_e2e.py --node /workspace/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/bin/node
```

Run ID: `sdk-e2e-7b7fb1b3a6e544eeb9cb2a6b38750484`.

| Task created through | Task ID | Status | Budget, micro-USD | Synthetic spent | Reserved | Unknown | Financial hold |
|---|---|---|---:|---:|---:|---:|---|
| Python SDK | `tsk_1b1076026ce3dc17c229d612f136a5f5` | succeeded | 10000 | 1000 | 0 | 0 | false |
| TypeScript SDK | `tsk_e5fb53feace3b55d9d9a578b20f0d7fc` | succeeded | 10000 | 1000 | 0 | 0 | false |

For **each** task, both Python and TypeScript independently read the task,
all four execution collections and the cost report. Both returned identical
counts: runs 1, operations 1, actions 0, approvals 0, reservations 1, ledger
entries 2, financial incidents 0. With `limit=1`, reading both ledger entries
required a real server-issued `next_entry_cursor`; the clients preserved it
and returned no duplicate records. Both root tasks had `depth=0`, matching
`root_id`, and no financial hold.

Both tasks produced:

```text
task.created → operation.claimed → cost.settled → task.succeeded
```

The Python task produced `art_46cefdae4056fc148f9833472cc6c5a4` (101 UTF-8
bytes); the TypeScript task produced `art_73818f17c1bfda3cba713325175f63e9`
(105 UTF-8 bytes). Both are version 1 `fixture-result.txt`, `text/plain`.
Both provenance objects identify `adapter=fixture:echo`,
`synthetic_cost=true`, `verified=true`, `verification=exact_goal_echo`, and
the corresponding operation ID. Every returned ledger entry retained
`synthetic=true`. No cost was written or adjusted through either SDK.

### Limits of this run

This is a real API → PostgreSQL → fixture worker → artifact → SDK read path.
It is not real-provider inference, billed cost verification, corporate identity
interoperability, a load test, or full-product acceptance. The successful
fixture contains no external actions, approval decisions, financial incidents,
unknown charges, overages or child tasks. Those endpoints' empty responses were
verified here; populated shapes and independent cursor query mapping are
covered by HTTP fixtures, while the integrator's storage/security suites own
the accounting state-machine and tenant-isolation evidence. Only the ledger
entry collection needed more than one live page in this scenario. The earlier
`E2E-EVIDENCE.md` remains the historical 0.1 run.
