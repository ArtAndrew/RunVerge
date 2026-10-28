# Independent contract evidence — 0.3.0

This is a new checkpoint for the breaking financial wire revision. The previous
`offline-report.json`, `live-report.json` and the 0.2 findings in
`docs/security/CONTRACT-REVIEW.md` retain their original historical meaning.

**Current status: gated offline and live HTTP acceptance for the implemented 0.3
contract surfaces.** The exact tested source hashes are in
`offline-report-0.3.json`. This checkpoint does not establish whole-product
readiness, provider billing correctness, browser SSO or a completed team/MCP
execution path.

| Check | Observed result |
| --- | --- |
| Draft 2020-12 syntax, references and DTO examples | PASS |
| Canonical financial decimal strings across full int64 range | PASS, including signed ledger minimum, maximum, values above JavaScript exact integer range, numeric rejection and noncanonical representations |
| Canonical state/error catalogs, policy and counter bounds | PASS |
| 15 declared method/path combinations, including safe `/models` | PASS offline and actual HTTP |
| AvailableModel exact safe field inventory and positive bounds; Task optional immutable profile pins | PASS |
| Quarantine/AccountingOutcome shapes and report pending/incomplete fields | PASS structural checks; not evidence of accounting acceptance |
| Generation drift of all five canonical generated JSON files | PASS; generation performed in temporary directory without rewriting contracts |
| SDK versions, Apache source markers and server-code dependency boundary guards | PASS; finite source checks, not an exhaustive legal provenance audit |
| Python offline suite | PASS, 20 test groups, 1.159s |
| Go independent semantic/state/permission tests with race detector | PASS, `tests/contracts` 1.296s |
| Go canonical domain tests with race detector | PASS, `internal/domain` 1.282s |
| Actual HTTP 0.3 server and both SDK examples | PASS; matching rebuilt server/worker, explicit fixture provider |

Reproduction commands and pinned Python test dependencies are described in
`README.md`. No new dependency was added to SDK runtime packages. SDK-specific
unit/build/type-consumer evidence is recorded separately in
`../sdk/CONTRACT-0.3-EVIDENCE.md` and `../sdk/offline-report-0.3.json`.

The tests initially identified a trailing-newline gap in a dollar-anchored money
regex and missing lower bounds on pending counters/base revisions. The canonical
generator owner repaired those constraints. Negative cases were retained; no
schema was weakened to make the tests pass. Final generated money patterns reject
trailing characters and represent factual int64 amounts without float conversion.
Policy admission budgets remain bounded numeric integers at at most 1e12.

Structural schema acceptance does not replace agent-reference authorization,
immutable published revision selection, uniqueness of team member IDs or the
exact two-member rule for delegate/verify. The same team fixtures deliberately
pass structural JSON Schema and fail canonical Go semantic validation. Schema
integer semantics also differ from the stricter Go lexical decoder for numbers;
duplicate JSON keys require checking before normal parsing collapses them.
UTF-8 byte-limit annotations document server limits rather than silently changing
standard JSON Schema string-length semantics.

The live probe rejects any health version other than 0.3 before creating
resources. Its new report target is `live-report-0.3.json`; it uses explicit
`fixture:echo` only. It requires nonempty actual fixture events, artifacts, runs,
operations, reservations and ledger entries, and separately reports counts for
possibly empty model/actions/approvals/incidents/quarantine collections. An empty
collection proves its envelope, not a nonempty feature execution path. No usage
observation is treated as authenticated final billing evidence.


The subsequent live run passed against health contract 0.3.0 on 2026-10-23,
covering all **15 unique method/path combinations** in the generated OpenAPI.
It validated actual success/error envelopes and malformed mutation rejection,
including duplicate keys, fractional/lexical-float integers and excessive UTF-8
name bytes. The fixture task succeeded with synthetic spent `"1000"`.
Actual counts: models 0, events 4, artifacts 1, runs 1, operations 1, actions 0,
approvals 0, reservations 1, ledger entries 2, incidents 0, quarantines 0.
The saved `live-report-0.3.json` includes response statuses, exact method/path
count and schema/source hashes. Empty collections establish envelope shape only.

Both actual SDK examples and their independent cursor reads also passed; their
separate evidence is in `../sdk/CONTRACT-0.3-EVIDENCE.md`. The implemented 0.3
surfaces therefore have consistent offline/semantic/live consumer evidence at
this checkpoint. This does not validate an absent MCP adapter, completed team
execution, browser SSO or externally authenticated billing ingestion.


Final-build live recheck: **PASS**, 2026-10-23T17:48:06.589172+00:00. After the integrator
restarted the main API with its final authorization fixes, both health-gated
live runners were executed again. All 15 contract routes and both complete SDK
examples/read inspections passed with the same stated collection counts and
synthetic monetary values. The version-specific live JSON files now contain
this final run and current source fingerprints. Offline and packaging checks
were not rerun; their recorded results remain unchanged. No production or test
source was modified during this final recheck.
