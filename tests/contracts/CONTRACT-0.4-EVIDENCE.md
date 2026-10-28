# Independent contract evidence — 0.4.0

This checkpoint extends the 0.3 checks; historical reports remain unchanged.
Offline acceptance is **PASS for the implemented schema/consumer surfaces**.
Live 0.4 acceptance is **PASS for all 20 declared routes** against the rebuilt
main API, PostgreSQL and worker. The health gate passed before mutations.

| Check | Observed result |
| --- | --- |
| Draft 2020-12 syntax, references, all DTO examples, strict mutation fields | PASS |
| Review decision enum, digest/version constraints, explicit true retry acknowledgment, input bounds | PASS |
| Optional cached-response execution metadata; fresh metadata required by UI before decisions | PASS |
| Team shape, member task/concurrency bounds, separate semantic examples | PASS |
| 20 public method/path combinations and envelopes | PASS offline |
| Exact financial strings and full int64 boundary/random checks | PASS, retained from 0.3 |
| Canonical catalogs/permissions and all five generated-file drift checks | PASS |
| Apache SDK source/dependency boundary guardrails | PASS; finite checks, not exhaustive legal provenance |
| Python schema tests | PASS, 21 groups, 0.989s |
| Go independent lifecycle/permission/team semantic suite | PASS with race detector, 1.278s |
| Canonical Go domain tests | PASS with race detector, 1.254s |
| Live HTTP 0.4 | PASS, 20 distinct method/path routes; actual synthetic team review/retry |

The independent negative example found that a `$`-anchored digest pattern
accepted a trailing newline under the schema validator. The generator owner
changed the canonical pattern to a strict end assertion; the negative case
remains and all three new digest-bearing DTOs reject trailing characters.
Lifecycle expectations explicitly add the 0.4 decision edges: Task waiting_input
to succeeded/failed/blocked; Run waiting_input to succeeded/failed; blocked
Operation to succeeded/failed. Terminal reopening remains forbidden.

Schema checks do not establish candidate ownership/current-run binding, content
bytes, actor rights, descendant consumption, a remaining lifetime deadline,
financial admission or root-only retry. Whitespace-only reasons, UTF-8 byte
limits and contextual feedback requirements are server controls. Team tests
separately demonstrate member-ID uniqueness, delegate/verify cardinality and
concurrency semantics; non-team context fixtures are not passed to ValidateTeam.
The actual transactional checks are covered by the integrator's separate storage
and independent security tests, not inferred from these schemas.

`run_live.py` targets 0.4 and preserves old report files. It is prepared to validate
all 20 routes through fixture:echo and a real team:coordinator with synthetic
participants, including request_changes, explicit retry/feedback, stale review
rejection, idempotency replay, acceptance and immutable decisions. That scenario
will not establish real-model quality, provider billing correctness, corporate
SSO or completed MCP support. Its actual execution passed at the live checkpoint below.

Commands and dependency isolation are in README.md; exact source fingerprints
are in offline-report-0.4.json. No SDK runtime dependency was added.


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
