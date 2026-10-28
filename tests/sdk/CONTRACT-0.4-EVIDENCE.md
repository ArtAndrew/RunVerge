# SDK and UI contract evidence — 0.4.0

Both SDKs implement review/retry and candidates/decisions/children reads, bounded
creation options and pinned team definitions. Task execution metadata and Run
attempt are optional for durable cached 0.3 responses; decisions require fresh
metadata. Financial facts stay exact canonical strings. No accounting mutation,
automatic transport retry or browser login is added to either SDK.

| Check | Observed result |
| --- | --- |
| Python local HTTP SDK tests | PASS, 27 tests |
| TypeScript local HTTP SDK tests | PASS, 33 tests |
| TypeScript build and public declaration consumer | PASS, including mandatory literal true cost acknowledgment |
| Python wheel and npm tarball | PASS: 11 / 28 files, no runtime dependencies, isolated imports and declaration consumer, no publishing |
| Web API/workflow/formatter tests | PASS, 13 tests |
| Web typecheck/build | PASS |
| Built browser UI with intercepted HTTP fixtures | PASS, 10 named scenarios, zero browser errors, 390px and desktop screenshots inspected |
| Offline 0.4 schema and independent Go semantic checks | PASS; details in ../contracts/CONTRACT-0.4-EVIDENCE.md |
| Actual 0.4 API/worker SDK review flows | PASS, both complete examples and both team review/retry flows |
| Actual built UI → API → PostgreSQL → fixture worker | PASS, no business response interception |

The web UI shows the draft before a decision, computes SHA-256 over the exact
rendered UTF-8 artifact, pins an open form to its current task/candidate/artifact
versions, and disables submission after polling observes a change. It records
request_changes without launching execution. New attempt has separate reason,
pinned feedback and an unchecked cost acknowledgment. The UI shows preserved
holds/spend, limits/deadline, decision history and child tasks. Child retry, missing
metadata, unexpected blocking reasons, hold/limit/deadline and tampered artifacts
are blocked. It does not call an agent verdict proof of truth or human presence.

The browser fixture covers request_changes → explicit new attempt → stale accept
form → fresh acceptance; unknown holds stay visible. Team form tests pin exact
published agent versions. Screenshots in web/.qa were inspected at 1440px and
390px; result columns fit without hiding overflow. The fixture uses fake local
credentials and intercepted responses, so its pass is not actual server/storage
or provider evidence. Its JSON report identifies that boundary explicitly.

Package checks built 0.4.0 distributions offline, inspected source/license/archive
boundaries, and imported all 20 public methods from isolated installed artifacts.
They also compiled public review/team/decision types and negative missing/false
acknowledgment examples. Nothing was published to a package registry.

Reproduction (supported Python 3.12 / Node 24):

```sh
python3 -m unittest discover -s tests/sdk -v
node packages/typescript-sdk/node_modules/typescript/bin/tsc -p packages/typescript-sdk/tsconfig.json
node packages/typescript-sdk/node_modules/typescript/bin/tsc --noEmit --strict --exactOptionalPropertyTypes --target ES2022 --module NodeNext --moduleResolution NodeNext packages/typescript-sdk/tests/public-types.ts
node --test packages/typescript-sdk/tests/*.test.mjs
node --test web/tests/*.test.mjs
python3 tests/sdk/check_packages.py --node /path/to/node24 --npm-cli /path/to/npm-cli.js
```

From web/, build first and run scripts/smoke-review.mjs with the installed
Playwright path and browser executable in WORKSPACE_PLAYWRIGHT_PATH and
WORKSPACE_BROWSER_PATH. Temporary HTTP listeners and Chromium need loopback
permission. Actual runners, after the 0.4 health gate is satisfied:

```sh
python3 tests/contracts/run_live.py
python3 tests/sdk/run_platform_e2e.py --node /path/to/node24
```

They read private bootstrap credentials into memory/environment, never report
tokens, and only use synthetic fixture participants. Existing 0.3 reports retain
their original meaning. Source fingerprints and offline results are in
`offline-report-0.4.json`; local distribution inventory is `package-report-0.4.json`.


## Actual 0.4 live checkpoint

Executed on 2026-10-23 at approximately 18:36 UTC against the main server after
migrations 009/010, with a real worker/coordinator. All three independent runners
passed on their first execution without a source fix. API health was 0.4.0.

The contract probe covered all **20 unique method/path combinations**, including
successful review/retry and malformed/stale request errors. Both SDK example
programs separately completed ordinary fixture tasks with synthetic spent
`"1000"`, reserved `"0"`, one immutable fixture candidate and no fabricated user
decision. Both SDKs independently inspected the execution/cost collections.

The contract probe, Python SDK, TypeScript SDK and browser each exercised a
separate fixture team: first candidate → request_changes → explicitly authorized
new attempt with pinned feedback → fresh candidate → accept. Each completed with
2 root attempts, 4 child tasks, 2 candidates and 2 decisions. Synthetic subtree
spend was `"2000"` before retry and `"4000"` afterwards; reserve was `"0"` at
completion. Request_changes alone launched no work. API and SDK probes replayed
the identical retry key and obtained the same cached result; stale candidate
acceptance returned 409. No money was submitted to a billing API.

The real browser runner used the built application and actual API/PG/worker;
it did not intercept business responses. It required an unchecked cost checkbox,
verified the displayed text digest and final history, displayed exactly $0.004,
and confirmed zero browser errors and no persisted token. Desktop/mobile
screenshots were inspected. The current team artifact displays raw coordinator
JSON with quoted inputs; it is inspectable but long on mobile and is not a
polished human-readable team summary. No production change followed this run.

Main model catalog, actions, approvals, incidents and quarantines were empty:
those live checks establish envelope shape only. The team and candidate objects
were nonempty. All participants were fixture:echo; these checks do not prove
real-model quality, provider bill reconciliation, corporate SSO, human presence,
or completed MCP support. Token-mode UI verification is separate from OIDC tests.

Live evidence and source fingerprints: `live-report-0.4.json`, plus
`../sdk/ui-live-report-0.4.json` for the browser (from the contract directory),
or `ui-live-report-0.4.json` (from the SDK directory). The raw browser report and
screenshots remain in `web/.qa/`. Historical 0.3 reports were not modified.


## Final-build recheck after explicit empty-member-task guard

All three existing live runners passed again on 2026-10-23 at approximately
18:45 UTC after the API/worker restart (READY2). The integrator had corrected a
wire discrepancy: an omitted team member task remains valid, an explicitly empty
one is rejected. Its independent negative regression is recorded separately.
These existing positive end-to-end runners do not substitute for that negative
case. No SDK, UI or test source was changed for this recheck.

Current `live-report-0.4.json` and `ui-live-report-0.4.json` carry the final-build
source fingerprints; earlier reports and browser screenshots were preserved with
the suffix `pre-member-task-fix`. Results remain 20 contract routes, both SDK
examples and both team review flows, plus the real built UI flow. Each team flow
has two attempts, four children, two decisions, synthetic spend 2000→4000 and
zero reserve; replay and stale409 checks still pass. Browser has zero errors and
no persisted token. Packaging and offline SDK tests were not repeated because
those sources were unchanged. Historical 0.3 evidence remains intact.
