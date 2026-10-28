# Independent contract evidence — 0.5.0

Offline acceptance is **PASS for the implemented schema and consumer surfaces**.
The initial offline checkpoint preceded deployment; the separate health-gated
actual 0.5 live probe now passes all 29 declared REST routes. Historical versioned reports remain separate.

| Check | Observed result |
| --- | --- |
| Draft 2020-12 schemas, local references and every DTO fixture | PASS |
| Material content presence, empty drafts, strict mutation fields and version bounds | PASS |
| Exact lowercase digest and rejection of trailing characters | PASS |
| Read-only current identity and 29 declared REST method/path routes | PASS offline |
| Exact int64 strings, canonical catalogs and five-file generation drift | PASS |
| Independent schema/license checks | PASS, 22 tests, 2.089s |
| Independent lifecycle/permission/team semantic checks | PASS with race detector, 1.192s |
| Canonical domain suite | PASS with race detector, 1.187s |

The new negative test found that a whitespace-only revision reason passed the
initial generated schema. The owner added a nonblank pattern matching Go's
whitespace set, including the same requirement for retry reasons. The original
negative case remains. The earlier semantic fixture for a whitespace retry
reason now records structural rejection as well as semantic rejection; the
test was strengthened rather than relaxing the new requirement.

Schemas permit empty and unfinished JSON draft content. Publication requires a
separate server check for nonblank content and valid JSON for JSON media; the
publication command itself contains only the exact saved revision/digest and
current parent/task binding. Merge-base relationships, current authorization,
head/task concurrency, frozen review basis and lifecycle cannot be established
by a structural schema pass. The examples make that distinction explicit.
Material revision versions are bounded to 128; exact artifact reads use a
positive version without introducing that artificial material bound.

The prepared live probe covers 29 routes and retains earlier fixture-team
review/retry checks, then saves a draft, detects a concurrent conflict, performs
an explicit merge, reads old versions, publishes an exact candidate without a
new run and accepts it. The separate SDK and real-browser probes exercise the
same implemented workflow. Prepared code is not a live result. Empty actions,
approvals, incidents, quarantine and model collections establish envelope shape
only when those paths are exercised. No model quality, provider billing,
corporate SSO, human presence, full MCP integration or whole-product acceptance
is inferred from this contract suite.

Commands and test-only dependency isolation are in [README.md](README.md).
Current source fingerprints are in offline-report-0.5.json (internal historical reference omitted).
No runtime dependency was added to the Apache SDKs. Their source/archive license
checks are finite guardrails, not an exhaustive legal provenance audit.

## Actual live checkpoint

The main local API reported0.5 after migration011. The existing probe passed
all29 method/path routes through actual API/PostgreSQL/worker with fixture-only
participants. It exercised nonempty materials/revisions, exact artifacts,
current identity, the optimistic conflict and manual merge, distinct exact
publication and candidate acceptance, plus retained review/retry behavior.
The material edit/publication created no new root run or attempt; synthetic
spend remained4000 after the earlier explicit retry and reserve stayed0.
Original versions and basis remained immutable. Empty model/actions/approvals/
incidents/quarantine collections still establish their envelope only.

The actual result and current source/build fingerprints are in
live-report-0.5.json (internal historical reference omitted). Separate SDK and real built-browser
evidence is in [SDK0.5 evidence](../sdk/CONTRACT-0.5-EVIDENCE.md). All use synthetic
fixture participants; no external model or bill was submitted.
