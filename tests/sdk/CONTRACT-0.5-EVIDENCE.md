# SDK and browser evidence — 0.5.0

Offline consumer acceptance is **PASS for the implemented material and team-view
surfaces**. The initial offline checkpoint preceded the main update. The separate
health-gated actual 0.5 API/PG/worker and built-browser checks now pass; their
results are recorded below.

| Check | Observed result |
| --- | --- |
| Python SDK local HTTP fixtures | PASS, 34 tests |
| TypeScript SDK local HTTP fixtures | PASS, 40 tests |
| TypeScript build and strict public declaration consumer | PASS |
| Web parser, material binding, review, API and financial formatting | PASS, 33 tests |
| Web typecheck and production build | PASS |
| Python wheel and TypeScript archive | PASS, 11 and 28 files; no runtime dependencies |
| Isolated package imports/declaration consumption and finite license-boundary guards | PASS; nothing published |
| Built team overview browser fixture | PASS; desktop and 390px mobile inspected |
| Built material editor browser fixture | PASS; desktop and 390px mobile inspected |
| Existing built review/retry browser regression | PASS with intercepted API responses |

Both SDKs expose metadata pages, exact immutable revision content, creation,
optimistic revision and separate publication. Empty and unfinished JSON drafts
can be saved. Local validation preserves the frozen byte/version/media bounds;
the server remains authoritative for publication, current parent binding,
permissions and lifecycle. Mutation keys remain explicit; no retry is added.
Artifact pages now support cursors, and exact artifact versions can be read
without assuming that the candidate is in the first page.

The team overview bounds bytes, depth, value/member counts and schema shape
before rendering. Participant text remains quoted and inert. Exact authoritative
artifact bytes and their computed digest remain accessible; member digests are
labelled as values from that artifact rather than independently verified facts.
Malformed input falls back to exact content access without claiming a valid
manifest. Child navigation is restricted to loaded direct children in the same
organization and parent. The overview does not prove truth or quality.

The editor fixture exercised save → concurrent revision → 409 with retained
buffer → explicit manual merge → immutable history → stale publication disabled
→ fresh exact publication → review. Publication changes the candidate without
a new run or financial change. A material candidate displays its edited text
separately from the original frozen team basis. The review snapshot captures
both rather than substituting a later poll response. Terminal drafts remain
editable, while publication is disabled. Hostile markup remains text.

Independent read-only review found no material defect in exact head/digest
binding, publication snapshot or plain-text rendering. It found that access
revocation originally discarded a dirty editor buffer. That gap was reproduced
and corrected with bounded, volatile App-owned memory. Eight buffers are retained
without automatic eviction; the sum of retained text fields is bounded to 262 KiB
and content to 256 KiB UTF-8. No local/session storage, IndexedDB, logs or automatic
post is used for that content. Reloading or closing the tab still loses the RAM
buffers, with an unload warning when available.

Recovery requires the exact normalized API URL, same principal and organization,
and fresh authorized material reads; a new unsaved draft requires a fresh task
read. Identity is checked both before those reads and immediately before the
local value is exposed. The browser fixture passed different-principal denial,
a principal switch during the material read, a denied resource read, same-user
rotated-token recovery, and explicit recovery of a new draft. The revocation
clearing policy remains in place. Changed saved versions require manual merge.
Logout/discard requires an explicit UI confirmation. These identity checkpoints
do not claim that a browser session can never change later.

Reports and source fingerprints: offline-report-0.5.json (internal historical reference omitted),
package-report-0.5.json (internal historical reference omitted),
[material browser fixture](../../web/tests/materials-report-0.5.json), and
[team browser fixture](../../web/tests/team-result-report-0.5.json).
Browser fixtures intercept business responses and do not establish actual
API/PostgreSQL/worker acceptance, provider behavior, billing, corporate SSO,
human presence or completed MCP integration.

## Historical local QA limitation

During development, one current mocked browser regression briefly wrote to the
old ignored `web/.qa/review-report-0.4.json` and its desktop/mobile screenshot
names. Those new outputs were moved to names containing
`0.5-pre-version-label-fix`, and the runner was corrected and rerun with 0.5
filenames. The original ignored detailed mocked-0.4 report and that image pair
were not tracked and cannot be restored from Git; they were not reconstructed.
The tracked historical 0.4 offline summary, its fingerprints, every actual live
0.4 report and live screenshots remain unchanged. This limitation does not
invalidate or relabel the separate historical live acceptance evidence.

## Actual main-server checkpoint

On 2026-10-24 the health-gated contract probe passed all **29** declared REST
method/path routes. Both SDKs passed ordinary fixture execution, execution/cost
inspection, team request_changes → explicit retry → accept, and separate editable
material workflows. Material publication preserved the root run and attempt 1,
original team basis and synthetic spend `"2000"`; reserve stayed `"0"`. Both SDKs
read immutable old versions, observed stale-edit409, replayed publication with
the same key, accepted revision3, saved a terminal draft revision4 and observed
terminal-publication409 without changing the accepted revision3. The initial
SDK run passed on stdout; an unchanged second run was captured in
live-report-0.5.json (internal historical reference omitted) for durable evidence.

The real built-browser material flow passed create → stale409 with retained
buffer → manual merge revision3 → separate publication → acceptance. Its root
run and attempt1 were preserved, one decision was recorded, original bytes and
basis remained, spend stayed2000 and reserve0. The separate real review/retry
flow retained its explicit cost acknowledgement: two attempts, four children,
two decisions and synthetic spend2000→4000. Both had zero browser errors, no
persisted token and **no business-response interception**. Recovery denial and
identity-swap tests remain separately labelled intercepted fixtures.

Reports: real material UI (internal historical reference omitted),
real review/retry UI (internal historical reference omitted), and
29-route contract (internal historical reference omitted). Current implementation,
compiled server and built asset fingerprints accompany these reports. Desktop
and 390px mobile captures were inspected and copied to
docs/evidence/ui-0.5 (internal historical reference omitted). No real model call or provider
billing was exercised. These local token-mode scenarios do not establish
corporate SSO, human presence or complete MCP support.
