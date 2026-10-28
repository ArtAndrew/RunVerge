# Explicit local model accounting and request bounds — API0.14

Status: bounded implementation verified in the current checkpoint (internal historical reference omitted).
No alpha/full-product acceptance.
This revision adds a trusted private profile mode and safe public observations;
it does not meter CPU/GPU, electricity or hardware depreciation.

## Private immutable mode

`ModelConnection.accounting_mode` may be omitted (historical metered), `metered`
or `operator_unmetered_local`. Omitted mode stays omitted when serializing the
private profile and PriceProfile into an immutable intent; display normalization
must never rewrite old stored profiles/hashes. New versions, not in-place edits,
change a profile's mode, price, endpoint or bounds. Existing tasks stay pinned.

Metered profiles retain positive `max_charge_microusd` and existing price checks.
Explicit local mode requires max charge, both token rates and fixed fee all zero,
`allow_insecure_loopback:true` and the exact canonical endpoint
`http://127.0.0.1:PORT/v1/chat/completions`, with a numeric port1–65535.
No userinfo/query/fragment/port alias/localhost/DNS/cloud endpoint is accepted.
Only the trusted operator selects this mode; an agent, peer, successful response
or the mere presence of a loopback address cannot select it implicitly.

Zero is the operator's API tariff declaration. A local daemon could itself
proxy to other services, so loopback is not proof of no upstream charge. The
operator must control its configuration. Hardware costs are not declared zero.

## Admission and uncertainty

The existing exact reservation/Action/intent, authority, root/org budget,
financial hold, fencing and one short dispatch permit remain mandatory.
The zero-amount reservation is created before I/O and retains its identity.
An uncertain/sent operation is not automatically retried or settled at zero.
Current permissions, inactive organizations, sticky holds and over-budget
ancestors can still deny `admit(0)`. Equality with a monetary ceiling can admit
a zero-priced call; paid tools and descendants still face their own/root caps.

Lifetime run/model-turn/tool counters, explicit consent for a new attempt,
task deadlines and input/output byte bounds remain. Input token count is a
declared profile assumption, not a consequence of a byte limit. Output token
limit is requested from the server and returned usage is validated. Client
timeout does not prove the server stopped computing. Worker concurrency is a
per-process bound; it is not a multi-worker/global physical-inference cap.
OS-enforced CPU/GPU/memory quotas remain separate incomplete scope.

## Public observations

`GET /models` returns required `accounting_mode` (`metered` or
`operator_unmetered_local`), `max_input_tokens`, `max_output_bytes` and
`timeout_milliseconds` alongside existing safe metadata. Metered max charge is
positive; local max charge is zero. Endpoints and credentials remain private.
These are current catalogue observations, not labels for a historical task.
New model artifacts record `provenance.model_accounting_mode` from that task's
exact immutable connection version; old artifacts need no invented default.

`CostReport.unknown_reservation_count` is a required canonical nonnegative
decimal string, bounded by signed-bigint maximum. It counts every unknown
reservation in the selected task's full subtree in the same repeatable-read
snapshot as totals, including zero amounts and rows outside the returned page.
The sum `unknown_microusd:"0"` can coexist with a positive count. Neither the
sum nor a page length proves that no unresolved operations exist. Existing
`incomplete` retains its quarantine meaning and is not repurposed.

SDKs preserve exact count strings. Workspace displays tariff assumptions,
request limits and uncertainty separately. Historical task labels use exact
candidate artifact provenance, not the newest catalogue entry with the same name.
API0.14 requires matching client headers/SDK/UI;0.13 clients are rejected before
mutations. This does not authorize upgrading the saved main installation.

## Acceptance evidence required

Explicit mode/rate/endpoint rejection, legacy profile/intent hash preservation,
real zero-price response and lost response, no resend, current authority and
financial holds, lifetime attempt exhaustion, paid-tool budget enforcement,
count beyond a reservation page, exact count decoding in SDK/UI, actual installed
local inference/restart and independent safety review. Broader resource quotas,
paid provider invoices, independent human usability and full recovery remain open.
