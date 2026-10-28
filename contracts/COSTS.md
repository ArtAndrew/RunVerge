# Cost contract 0.3.0

Owner: integrator. Source types: `internal/domain/execution_types.go`. Source implementation: `internal/storage/ledger.go` and `accounting.go`. Amounts are exact integer micro-USD. Admission reserves and policy budgets are bounded by 1,000,000,000,000; final factual amounts support 0…9,223,372,036,854,775,807. Factual wire fields use canonical decimal strings; signed journal deltas also use strings. Values outside int64, other currencies and partial billing observations are explicitly unsupported. No midnight reset or implicit invoice is performed.

## Identity and authority

An Operation is logical work. A Reservation/Action represents one potentially charged physical attempt. A queue replay of that attempt uses its existing charge key; another possibly paid attempt requires another independently admitted reservation. `charge_key` is unique per organization and bound to task, operation, amount, category, immutable provider-account identity, pricing revision and intent digest. Changing any of these under the same key conflicts. Provider means a stable configured account binding, not just a vendor brand or a request-provided tenant name.

Only trusted kernel/gateway accounting calls ReserveCost/PrepareModelDispatch/MarkCostUnknown/AccountCost/AccountAdjustment/ResolveCostQuarantine/ReleaseUnsentCost. **There is no public HTTP/MCP accounting mutation endpoint.** A model, ordinary task token or sandbox process cannot assert a zero bill, refund itself or submit an arbitrary source. An eventual remote billing ingress must authenticate provider/account bindings before constructing CostEvidence; its JSON body alone is not evidence. Current tests use explicitly trusted cost fixtures, not real provider invoices. Legacy SettleCost/AdjustCost wrappers return a typed QuarantinedError after durable acceptance; new ingestion must inspect AccountingOutcome instead of treating quarantine as a failed or free attempt.

## Hierarchy

Task.parent_id, root_id, depth, owner and pinned agent identity are immutable. Root depth is 0; max depth is 16. Every child remains in its parent's organization and inherits the authority owner. Team/delegation actors can differ without changing budget ownership; their effective capabilities must be intersected with the parent at the runtime boundary. A child's budget is a constraint, not a prepayment.

One reserve updates the organization and every root-to-leaf task projection atomically. Frozen scope_task_ids come from stored lineage, never a client list. A canonical journal entry exists once per economic change. Adding root, child and organization projections together would double-count the same expense and is forbidden for metering/invoicing.

Example: root limit 1000, siblings limited to 700 each. Two concurrent holds of 600 cannot both succeed. A root cost report lists one canonical reservation for each physical attempt in the subtree.

## Admission and dispatch

1. Lock the immutable owner principal and membership for SHARE before execution/account locks. Revocation/identity mutations must lock all relevant authority rows in stable order before operation/account locks. This serializes committed admission against revocation.
2. Lock existing operation; then organization; root-to-leaf task rows; reservation; action. Trusted accounting skips authority checks but uses the same operation/account/scope order.
3. Check live lease using database `clock_timestamp()`, current authority, ancestor state, financial holds and every budget. Persist reserve, immutable Action intent, journal, state and outbox atomically.
4. Before an external model call, PrepareModelDispatch locks all relevant rows, then atomically changes pending Action to sent only while the lease remains live. It persists a bound, immutable dispatch receipt with a deadline no later than the lease expiry or two seconds after admission. Explicit action-row locking precedes the final clock check. A repeated dispatch is rejected.
5. Commit before network I/O. A committed sent intent is the admission point. The runtime conservatively maps the returned duration onto a monotonic clock captured before admission; time spent waiting or committing consumes that duration. The transport enforces the start deadline through connection/TLS and the first application write, separately from response timeout. Revocation/cancellation after admission cannot guarantee rollback of already escaped bytes. The gateway has no automatic retries.

`now()` is unsuitable for lease validation after waits because PostgreSQL pins it to transaction start. A worker can lose its lease while waiting for an account, reservation or action lock; the final transition must reject that stale permit.

## State changes

| Current state | Input | Result |
|---|---|---|
| None | Reserve valid attempt | reserved; hold added in all scopes; pending Action saved |
| reserved | Prepare dispatch | sent Action; cost state stays reserved |
| reserved/unknown | Mark unknown | unknown; held amount unchanged; uncertainty appended |
| reserved/unknown | Authoritative final CostEvidence | settled; hold removed; full actual added |
| reserved/unknown, Action pending | Release unsent | released; Action rejected under lock; hold removed |
| sent/unknown Action | Release unsent | conflict; timeout/lease expiry is not proof of zero cost |
| settled | Same final event replay | no duplicate expense; current accounting result returned |
| settled | Changed final fact | conflict; explicit versioned correction required |
| settled | AdjustCost with expected revision and new evidence | append signed delta, update net actual; never edit history |
| released | Late authentic final charge | settled plus incident; original scope debited fully |

CostEvidence requires `final:true`, `currency:"USD"`, the admitted source, a unique source/reference, a lowercase SHA-256 payload digest and a nonnegative final actual. Adjustments also require a reason and expected revision. Different event IDs cannot turn one already-settled attempt into two expenses. A replay of an older accepted event returns the current reservation without undoing a later correction.

Unknown holds survive cancellation, role revocation, lease loss, process restart and task terminal states. Trusted settlement still records incurred cost after those events and never resurrects execution or publishes a result. Confirmed zero actual is settled with evidence; it is distinct from assuming that an unanswered request was free.

## Overrun and history

Admission checks `limit - spent - reserved >= new_hold` for every scope. Actual facts are not constrained by the former admission bound. If actual exceeds the hold, or a late charge arrives after release, the system records the **full** actual, a financial incident and sticky financial_hold on affected scopes. Further admissions stop. Already dispatched costs can still settle.

If adding a supported fact would overflow any int64 counter, AccountCost/AccountAdjustment atomically retains immutable evidence, creates a durable quarantine, marks the affected scopes held and appends the incident/event. It returns `status:quarantined`; the fact has been accepted. No projection is partially updated, no spent amount is fabricated and an existing hold is retained. A pending quarantine blocks conflicting facts for that reservation. Exact replay returns the same accepted outcome. A different reservation/source binding or changed payload conflicts.

ResolveCostQuarantine uses only the stored fact and an expected revision. While the projection still cannot fit, it leaves the quarantine pending without churning revisions. If it fits, one transaction applies the delta, appends an immutable resolution and clears the pending link. This never clears sticky financial holds or restarts work. A released attempt does not acquire an invented hold; its pending liability remains explicit through quarantine and incomplete summaries. Resolution and accounting replay survive cancellation and revoked execution authority.

The incident currently includes the reported actual amount; the difference from the hold and account limit is derived from the immutable reservation and projections. A financial_hold is not cleared automatically by a refund. An authorized administrative reconciliation process must handle that separately.

Ledger entries, evidence and incidents are append-only. Updates/deletes are rejected by database triggers. Referenced tasks, scope IDs and deduplication records cannot be hard-deleted; future content-erasure/archival must keep accounting tombstones. No retention expiry is promised in this version.

The synthetic `fixture_legacy` adapter has a separate, known per-logical-operation test price. Its reaper filters that category explicitly and cannot settle/free model/tool/compute/verification/recovery liabilities. Fixture events are labelled synthetic in the journal and UI.

## Read API

`GET /tasks/{id}/costs` returns one repeatable-read database snapshot of subtree balances, unknown exposure, financial_hold and four bounded collections. `limit` defaults to 50, max100. Independent query cursors: `reservation_cursor`, `entry_cursor`, `incident_cursor`, `quarantine_cursor`; corresponding output cursors begin with `next_`. Each collection sorts descending by immutable created_at/id. Quarantines include the supported factual amount and pending/resolved status, without provider evidence references/bodies. `pending_quarantine_count` covers the whole subtree; `incomplete:true` means balance projections do not yet include every accepted fact. A consumer must not present such balances as a complete bill or sum huge pending facts into an overflowing counter.

This contract defines lifetime provider-cost attribution. Customer tariffs, billing periods, signed billing webhooks, real provider reconciliation and commercial invoicing remain explicit later work, rather than being inferred from a test echo response.
