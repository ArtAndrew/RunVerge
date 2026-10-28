# Live SDK fixture verification

Historical contract 0.1 run. The subsequent successful API 0.2 execution and
accounting-read scenario is recorded in [CONTRACT-0.2-EVIDENCE.md](CONTRACT-0.2-EVIDENCE.md).

Executed on 2026-10-23 against the running local server at
`http://127.0.0.1:8080`, with its real PostgreSQL-backed API and fixture worker.
Python 3.12.14 and Node.js 24.19.0 ran the actual example programs.

This verifies the HTTP-to-worker integration using **`fixture:echo` only**.
The reported costs are synthetic accounting entries, not billed inference.
This does not demonstrate real model quality, external actions, or production
readiness.

## Exact executed command

Working directory: the repository root.

```sh
/workspace/.cache/codex-runtimes/codex-primary-runtime/dependencies/python/bin/python3 tests/sdk/run_platform_e2e.py --node /workspace/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/bin/node
```

Exit code: `0`. The runner reads `.local/bootstrap.json` into memory and passes
the token through child-process environments; neither the command nor this
report contains credentials. A loopback-capable execution environment is
required. Equivalent invocation on a machine with the runtimes in `PATH`:

```sh
python3 tests/sdk/run_platform_e2e.py --node node
```

The opt-in runner is not part of ordinary unit-test discovery. It creates one
fixture agent and task per SDK per invocation. It executes these real examples
with generated idempotency keys and independently reads their server state:

- `examples/python/fixture_task.py`
- `examples/typescript/fixture_task.ts`

## Recorded result

Run: `sdk-e2e-791f599bb962449bba0153fe64cc29aa`.

| SDK | Task | Status | Budget, micro-USD | Reserved | Synthetic spent |
| --- | --- | --- | ---: | ---: | ---: |
| Python | `tsk_705cf356ac117230c15d4444c0712a2c` | succeeded | 10,000 | 0 | 1,000 |
| TypeScript | `tsk_837404d269bca50509348d89b0d64b37` | succeeded | 10,000 | 0 | 1,000 |

Both tasks produced the durable event sequence:

```text
task.created → operation.claimed → cost.settled → task.succeeded
```

| SDK | Artifact | Media type | Version | Content size |
| --- | --- | --- | ---: | ---: |
| Python | `art_9d9cfc09e489f3eb8add6fbe738e12cf` / `fixture-result.txt` | text/plain | 1 | 101 UTF-8 bytes |
| TypeScript | `art_2aad7cf440e44cfe14610e0f3e20ce10` / `fixture-result.txt` | text/plain | 1 | 105 UTF-8 bytes |

Both artifacts had verified provenance:

```json
{
  "adapter": "fixture:echo",
  "synthetic_cost": true,
  "verification": "exact_goal_echo",
  "verified": true
}
```

The runner asserts success, nonempty artifacts, the provenance fields above,
zero outstanding reservation, and exactly 1,000 synthetic micro-USD settled per
task. It also emits task and artifact IDs and event types for inspection.
