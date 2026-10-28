# SDK contract 0.3 evidence

This is new evidence for the breaking 0.3 financial wire update. Historical 0.1/0.2 evidence files have not been relabeled. The source hashes and base commit for the independent SDK runs are in `offline-report-0.3.json`; changes are still in the shared working tree.

Implemented surfaces: Python `Money = str` / `parse_micro_usd`, TypeScript `Money = string` / `parseMicroUSD -> bigint`, canonical full int64 validation without floats, signed ledger deltas, strict task financial facts, nested report monetary fields, quarantine collection/count/incomplete status/cursor, and the organization model catalog (`list_models` / `models`). The SDK has no accounting submission or adjustment method and does not implement browser SSO. Server policy budgets remain bounded numbers. Public type declarations require the contract 0.3 Task fields.

| Check | Observed result |
| --- | --- |
| Python stdlib SDK HTTP + exact-money tests | PASS, 21 tests |
| TypeScript SDK build | PASS |
| TypeScript public declaration consumer compile | PASS |
| TypeScript HTTP + exact-money tests | PASS, 27 tests |
| Web API/formatter tests, including exact huge/negative amounts | PASS, 6 tests |
| Full regenerated 0.3 schema acceptance | PASS, 20 offline schema groups + Go canonical semantic/domain checks |
| Live server/SDK examples with 0.3 | PASS, both actual examples and both clients' read/cursor checks |
| Local distribution packaging | PASS, Python wheel and npm tarball, isolated imports and TypeScript declaration consumer |

Commands run from the repository root (Python 3.12, Node 24; local HTTP servers require loopback permission):

```sh
python3 -m unittest discover -s tests/sdk -v
node packages/typescript-sdk/node_modules/typescript/bin/tsc -p packages/typescript-sdk/tsconfig.json
node packages/typescript-sdk/node_modules/typescript/bin/tsc --noEmit --strict --exactOptionalPropertyTypes --target ES2022 --module NodeNext --moduleResolution NodeNext packages/typescript-sdk/tests/public-types.ts
node --test packages/typescript-sdk/tests/*.test.mjs
node --test web/tests/api.test.mjs
```

The HTTP tests use local fixtures with test-only credentials. They exercise maximum int64, values just above JavaScript's exact integer range, both signed bounds, numeric wire rejection, noncanonical forms (`-0`, leading zeros, exponents, whitespace/newline), nested fields and independent quarantine pagination. A malformed response produces a protocol error without retry. Monetary facts are preserved as wire strings; callers opt into exact `int`/`bigint` arithmetic. The web formatter formats integer/fraction components without converting financial strings to `Number`.

The updated opt-in runners check a 0.3 health response before creating fixture resources. `tests/contracts/run_live.py` writes `live-report-0.3.json`, preserving prior evidence. `tests/sdk/run_platform_e2e.py` runs the actual examples with credentials in memory/environment, then inspects task/artifact/execution/accounting state through both SDKs. A fixture pass establishes neither real-model inference nor paid billing reconciliation.


Live 0.3 execution completed on 2026-10-23 against the rebuilt local API and
worker with migrations 001–008. `live-report-0.3.json` contains the two actual
task IDs, artifact IDs/provenance, exact financial strings, read counts and source
hashes. Each language created one `succeeded` fixture task with one nonempty
artifact, 4 events, 1 run, 1 operation, 1 reservation and 2 ledger entries.
Both clients independently traversed these task resources with `limit=1`;
the ledger traversal used a real server-issued cursor. Each task has synthetic
spent `"1000"`, reserved `"0"`, unknown `"0"`, no financial hold, zero pending
quarantines and `incomplete=false`.

The actual model catalog was also read through Python `list_models` and
TypeScript `models`; both returned zero items. Actions, approvals, incidents and
quarantines were likewise empty. These results establish the live envelopes,
not populated execution behavior for those collections. Populated money and
quarantine cases remain separate HTTP-fixture and database evidence.

Exact live commands, with credentials read only in memory/environment:

```sh
python3 tests/contracts/run_live.py
python3 tests/sdk/run_platform_e2e.py --node /path/to/node24
```

The Python run used the bundled Python 3.12 executable and the SDK example run
used the bundled Node 24 executable. The updated read helpers were then rerun
against the same existing task IDs to check the model catalog, without creating
additional tasks.

`check_packages.py` builds both distributions locally and offline without a
registry upload. The Python 0.3.0 wheel contains 11 files, including `money.py`,
`py.typed`, metadata and Apache license, and declares no runtime dependencies.
An isolated Python process imported the installed wheel, all public accounting
and model types, all 15 client methods, and exact money parsing at both int64
bounds. The TypeScript 0.3.0 tarball contains 28 files; `npm pack --offline` runs
its prepack build. An isolated consumer imported the installed package, exercised
exact bigint parsing and all client methods, and compiled its public declarations
under strict TypeScript settings. Archive inventories and hashes are preserved
in `package-report-0.3.json`. Archive path/source checks found no server modules,
private files or Elastic source markers; these are finite guardrails rather than
an exhaustive legal provenance audit. Nothing was published.

```sh
python3 tests/sdk/check_packages.py --node /path/to/node24 --npm-cli /path/to/npm-cli.js
```

The live fixture and packaging passes do not establish paid-provider inference,
final-bill reconciliation, browser SSO or whole-product acceptance. Prior 0.1/0.2
reports remain unchanged.


Final-build live recheck: **PASS**, 2026-10-23T17:48:06.589172+00:00. After the integrator
restarted the main API with its final authorization fixes, both health-gated
live runners were executed again. All 15 contract routes and both complete SDK
examples/read inspections passed with the same stated collection counts and
synthetic monetary values. The version-specific live JSON files now contain
this final run and current source fingerprints. Offline and packaging checks
were not rerun; their recorded results remain unchanged. No production or test
source was modified during this final recheck.
