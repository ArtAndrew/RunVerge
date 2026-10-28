# Independent contract checks

Scope: contract revision 0.5.0. Test dependencies are isolated in ignored `.deps/`; SDK runtime dependencies are unchanged. Run with Python 3.12+.

```sh
python3 -m pip install --target tests/contracts/.deps -r tests/contracts/requirements-dev.txt
python3 tests/contracts/test_contracts.py
```

The checks validate Draft 2020-12 schemas, local references, representative good/bad DTOs, required/null/unknown fields, status/error enums, money/counter/team bounds, permission boundaries, UTF-8 byte-limit annotations, HTTP response envelopes, SDK source/license markers and exact generation drift for all five generated files. The generator is compiled from the current Go source, then run in an isolated temporary working directory; these tests never rewrite `contracts/`.

The schema validator explicitly enables date-time checking and refuses external reference retrieval. Official APIs were reviewed in the [jsonschema validation documentation](https://python-jsonschema.readthedocs.io/en/stable/validate/) and [reference registry documentation](https://python-jsonschema.readthedocs.io/en/stable/referencing/). No network service is needed after dependency installation.

Run canonical semantic checks, all six lifecycle state-pair matrices, permission-denial checks and the independent team fixtures with the installed Go environment:

```sh
python3 - <<'PY'
import subprocess
from scripts.dev import environment
subprocess.run(['go', 'test', '-mod=readonly', '-race', '-count=1',
                './tests/contracts', './internal/domain'],
               env=environment(), check=True)
PY
```

`fixtures/semantic-examples.json` deliberately contains teams that pass structural validation but fail domain validation: duplicate member IDs and a delegate/verify team with only one member. JSON Schema cannot by itself verify that an agent exists, that its immutable revision is published, or that the caller can use it. Schema success is not authorization, semantic acceptance or dispatch permission. A structurally valid AgentDefinition does not establish that its referenced published plan can execute. Additional context examples cover cross-task candidate binding and whitespace-only reasons; those require storage validation, not the team domain validator.

JSON Schema integer types describe mathematical values: `10000.0` is an integer under this standard. The Go wire decoder additionally rejects floating-point/exponent notation for integer DTO fields. The schema also cannot detect duplicate object members after a parser has already collapsed them. Live malformed-JSON checks exercise those separate wire controls; no custom validator silently changes standard Schema semantics. UTF-8 byte limits and database/referential guards remain server responsibilities where the schema documents only structural constraints.

For a rebuilt server and fixture worker using current contract 0.11.0, with loopback permission:

```sh
python3 tests/contracts/run_live.py --preflight-only
python3 tests/contracts/run_live.py
```

The preflight reads only the health revision, before credentials or fixture manifests, and creates no records or reports. It does not verify permission or peer connectivity. The full probe reads `.local/bootstrap.json` in memory, never reports the credential, disables proxies and redirects, checks the health contract before mutations, and uses only `fixture:echo` participants and `team:coordinator`. It creates a labeled agent/task, validates actual endpoint responses, waits for fixture completion and checks malformed commands. The default full run writes `.local/qa/contract-live-report.json` (choose a new `--report` path for each run); historical `live-report.json` and `offline-report.json` remain evidence of their original 0.2 run. Its JSON report contains only route/status/count observations and synthetic spend. A server with an earlier contract is rejected before the probe creates resources. Empty actions/approvals/incidents/quarantines collections validate only their envelope; nonempty DTO behavior is covered by the separate examples until a real feature path produces those objects.

For W01 acceptance, record all three checks, the exact revision, actual live counts and remaining gaps in `docs/security/CONTRACT-REVIEW.md`. Do not mark W01 complete while schemas, canonical validators, runtime wire responses or consumer revision claims contradict each other. These checks do not establish complete MCP support, browser SSO, whole-product readiness or an exhaustive legal provenance audit.

Contract 0.3 tests require canonical decimal int64 strings for financial facts,
including boundary/random exact values, signed ledger deltas, trailing-newline
rejection, quarantine DTOs and the safe `/models` catalog. Policy budgets remain
bounded numbers. Schema evidence does not prove pending facts were booked,
that a model route is authorized, or that the configured price assumptions are
true for an external provider. No billing mutation is exposed in either SDK.

Contract 0.5 adds immutable material revisions and separate publication. The
live probe covers a fixed subset of the current catalog and preserves the original team artifact, saves an empty draft,
checks a stale revision conflict and explicit manual merge, reads old versions,
publishes an exact revision without a new execution, then reviews the resulting
candidate. Draft content may be empty or unfinished JSON; nonblank/valid JSON
publication, fresh parent binding and merge-base cross-field relationships remain
server controls. Material metadata pages do not contain revision text; exact
content is fetched by version. Artifact listing is paginated and exact candidate
artifacts are read directly when outside the first page. Historical versioned
reports are retained; a prepared runner is not itself evidence of a live pass.
