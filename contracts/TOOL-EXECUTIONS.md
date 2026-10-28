# External tool execution contract 0.6.0

This bounded profile supports an explicit authorized API/SDK/MCP request to a registered HTTPS tool. It does not yet enable autonomous model `tool_calls`, agent tool pins, arbitrary native tool protocols or stdio code execution. Those remain part of the complete product scope. Only configured `http_json` and `mcp_https` adapters using `agent_platform_action_v1` are admitted here.

## Public routes

All paths are relative to `/api/v1`. Current authorization, browser CSRF, strict mutation fields/Unicode, exact queries and scoped durable `Idempotency-Key` apply.

| Route | Result |
|---|---|
| GET `/tools?cursor=...&limit=...` | Safe ToolInfo metadata, default50/max100 |
| GET `/tasks/{id}/tool-executions?cursor=...&limit=...` | ToolExecution metadata, arguments omitted, default20/max100 |
| GET `/tasks/{id}/tool-executions/{execution_id}` | Exact ToolExecutionView |
| POST `/tasks/{id}/tool-executions` | PrepareToolExecution →201 ToolExecutionView |
| POST `/tasks/{id}/tool-executions/{execution_id}/decide` | DecideToolExecution →200 ToolExecutionView |
| POST `/tasks/{id}/tool-executions/{execution_id}/dispatch` | DispatchToolExecution →200 ToolExecutionView |
| POST `/tasks/{id}/tool-executions/{execution_id}/reconcile` | ReconcileToolExecution →200 ToolExecutionView |

The exact view contains `execution`, the pinned safe `tool`, its `approval`, current `task`, `available_commands` and optional immutable `receipt`. Lists contain metadata, not an approval-ready preview. Use the exact view before a decision; never find authority by guessing a first page.

ToolInfo contains an ID/version, name/description, declared effect (`read`, `write`, `destructive`), safe destination label, provider/account binding reference, price revision, exact maximum exposure string, input-schema string/digest, distinct-principal policy and size/time bounds. It contains no endpoint, secret reference, credential or CA bundle. Provider/account labels and price bounds are operator declarations; compatibility and account verification for real providers still require provider-specific acceptance.

## Preparing an occurrence

Input: `{occurrence_id,expected_task_version,candidate_id,artifact_id,artifact_version,content_digest,tool_id,tool_version,arguments_json,expires_in_seconds}`.

`occurrence_id` matches `[A-Za-z0-9][A-Za-z0-9._:-]{0,127}` and is unique in an organization/Task for its entire lifetime, across Runs and terminal outcomes. An identical idempotency replay returns its durable original response after current permission checks. A different HTTP key cannot reuse an occurrence ID to create another physical action. A deliberately different business occurrence requires a new ID, a new approval and a separate cost acknowledgment. It is never an automatic retry of an unknown effect.

`arguments_json` is the exact original UTF-8 JSON object string, at most32768 bytes. Duplicate properties, invalid Unicode/NUL, excessive depth and schema violations are rejected. No float64 conversion, whitespace normalization, default insertion or key reordering precedes its SHA-256. Large integer argument bytes are preserved. The current bounded schema profile supports specified object/scalar/array constraints; refs, combinatorial schemas, defaults and unsupported assertions are rejected at trusted installation. It is not a claim of complete JSON Schema support.

Preparation requires a current waiting-input model/team/material candidate with exact Task version and candidate/artifact/digest binding, a blocked source Operation, no active subtree work or result decision and an active immutable tool profile. A managed child already consumed by its parent cannot prepare an action. Structural writers share the permanent tree gate, so preparation and parent selection cannot pass one another unchecked.

The server creates a fresh financial Action/reservation, ToolExecution and pending Approval; reserves the profile's maximum exposure across every ancestor and organization; and moves the existing Task/Run to `waiting_approval`. It performs no external call and creates no new Run. The source Task version is the pre-reservation CAS version; current Task version can advance for both financial and workflow updates. Clients use the returned `view.task.version`, not an assumed increment.

An intent hash binds immutable IDs, occurrence, Task/Run/Operation/candidate/artifact snapshot, argument string/digest, pinned connection-profile digest, declared price/exposure, creator and exact expiry. The connection digest excludes only current activation state and creation timestamp; it includes the configured endpoint, transport, secret reference, trust bundle and limits. Credential key material is not stored in the intent or exposed to the caller. Trustworthy account-scoped credential brokering remains additional full-product work.

## Approve, deny, then explicitly dispatch

Decision input: `{expected_task_version,expected_execution_version,expected_approval_version,intent_hash,decision,reason}`, where decision is `approve` or `deny` and reason is nonblank, at most4096 UTF-8 bytes. It records authenticated principal/time/method `authorized_principal_decision`. The method does not establish human presence. Distinct approval means distinct principals, not independently proven humans.

Admin/owner have `actions.approve_any`; member cannot approve a shared tool account. Approval requires current owner/creator authority, an active matching profile and unexpired binding. If `require_distinct_approver` is set, the creator cannot approve; destructive profiles require that policy. An explicitly configured single-principal profile may permit an admin to approve their own request. It is not four-eyes assurance.

Safe denial is also allowed for a retired profile, self-created distinct-principal request, or revoked owner/creator: it makes no new external call. Exact current actor permission and snapshot checks still apply. A provably unsent pending Action releases its reserved/unknown reservation exactly once. Existing settled/quarantined/sent exposure is not inferred away. The original candidate returns to waiting-input; denial neither rejects nor accepts that candidate.

Approval does not queue a call. Dispatch input separately requires `{expected_task_version,expected_execution_version,expected_approval_version,intent_hash,acknowledge_cost:true}`. It rechecks the current source binding, actor/owner/creator/approver authority, active profile, expiry and ancestor/org budget/holds; records `dispatched_by`; and queues a separate tool job while Task remains waiting-approval. It performs no call inside the transaction.

`available_commands` contains individual `approve`, `deny`, `dispatch`, `reconcile` hints computed from current server state. The UI must gate each decision separately. These hints do not replace command validation or guarantee that state stays unchanged after the read. A stale modal must not silently substitute a new version/hash;409 requires opening the updated snapshot explicitly.

## Worker and external protocol

The job has its own lease, fencing token and at most5 claims. It reuses the blocked source Operation for attribution, not its model-execution lease. It cannot be claimed by the model/fixture executor. Claim/renew/final send check current principal/membership roles, requester of dispatch, source tuple, profile, active root state and expiry after lock waits. Billing changes to Task version do not invalidate an unchanged source tuple forever.

The final send transaction marks the physical Action sent and issues one nonrenewable permit of at most2 seconds. The adapter measures admission latency from monotonic time before the database call and checks the remaining deadline before the first application write. Lost commit acknowledgment does not authorize a network call. TLS/DNS/discovery do not renew the permit. The HTTP action mode disables transport retry, redirect, proxy, body replay, keep-alive and HTTP2; each POST uses the same absolute deadline. For MCP, delayed discovery cannot authorize a later tools/call after expiry.

For the existing `agent_platform_action_v1` result contract, the configured HTTPS endpoint receives exactly:

```json
{"execution_id":"tex_...","action_id":"act_...","intent_hash":"sha256","arguments":{"configured":"business fields"}}
```

`Idempotency-Key` is the stable Action ID. It is an interoperability aid, not permission for an automatic resend. This result contract must return the same execution/action/intent and `{outcome:"succeeded"|"rejected"|"unknown",effect_reference,result:string}`. Its MCP transport uses the same envelope and structured-content result contract. Generic HTTP200, MCP `isError:false`, peer annotations or an arbitrary textual claim are insufficient. A bounded valid matching registered response is recorded as method `registered_tool_response`, an attributed observation under that configured trust contract; it does not prove arbitrary factual truth.

The additional host-installed [`mcp_read_text_v1`](MCP-READ-TEXT.md) profile is limited to `mcp_https` with effect `read`. It sends ordinary approved MCP arguments and preserves1..16 text blocks as a lossless JSON string array. A valid `isError:true` is an observed read error (`rejected`); success means observed data. Neither result accepts the task, verifies factual truth or settles money. Peer annotations never select this profile or grant authority. Write/destructive contracts remain unchanged. This profile currently requires the pinned2026-07-28 protocol and an explicitly configured stateless HTTP server; legacy stateful servers are outside its verified support profile.

Only operator-installed endpoints/tool names and explicit credentials are used. TLS verification, pinned optional private CA, exact network destination and per-connection DNS policy apply. Metadata/link-local/unspecified/multicast targets, redirects and ambient proxy are disallowed. An explicit private-network profile permits organization-managed/loopback endpoints; it is not an egress firewall. Secret material is resolved by the worker from a restricted environment reference, not user args, tool descriptions, an incoming platform token or a prompt. Arbitrary executables/stdio remain unavailable until a tested OS boundary is implemented.

## Effects, money and recovery

ToolExecution effect state is separate from the financial Action/reservation. A final invoice can settle money before the effect is known. A matching tool result can be observed while cost remains unknown. Neither substitutes for the other.

Once possibly dispatched, timeout, malformed/oversized response, redirect, mismatched IDs, missing contract or lost response preserves unknown effect and liability. There is no automatic replay, model retry or replacement occurrence. The Task is blocked with `external_action_unknown`. A proof of no dispatch requires locked pending Action with no dispatch marker; it can release even a conservatively unknown reservation when admission actually rolled back. A cancelled/expired worker is not that proof by itself.

A trusted late observation with the exact durable sent fence can append a receipt after lease loss, cancellation or revocation. It cannot reopen a cancelled Task, accept a candidate or change money. Current matching nonterminal Task returns to its original candidate waiting-input only after the effect has a terminal observation. The result is a fresh immutable version1 Artifact with unverified provenance, separate from the candidate; the original source text/Run/candidate/decisions remain unchanged.

An admin/owner can explicitly reconcile an unknown effect with `{expected_task_version,expected_execution_version,intent_hash,outcome,effect_reference,result,evidence_digest,reason}`. Result may be empty, up to262144 bytes; reference is nonblank≤256, evidence is64 lowercase hex and reason is nonblank≤4096. It records method `operator_assertion`, including principal and evidence. It is not provider verification, performs no network call and never settles/releases money. Revoking the original owner does not prevent a currently authorized administrator from recording this observation.

A later contrary admitted observation cannot overwrite that first receipt. Up to8 immutable conflicting observations per execution retain exact result artifacts/digests/evidence and appear in `tool.outcome_conflicted` events; identical repeats deduplicate. A single durable overflow event states that further payloads were not retained and operator review is required. No contradiction makes an Action dispatchable, clears a bill, changes the Task result or triggers another occurrence. Full provider readback/corrections/retention workflows remain additional work.

## Limits and persistence scope

One open effect per Task;32 total occurrences per Task and128 across its root tree, including denied/expired/zero-cost preparations and later Runs. Expiry is1..86400 seconds, capped by the Task deadline. Saved counts are monotonic, so retries do not reset them. Tool input≤32768 bytes, schema≤8192, output≤262144 and configured call timeout1..30 seconds; policy maximum charge is a canonical decimal string0..1e12 micro-USD. Unknown spent/reserved facts continue to use the existing full-range financial contract.

State, approvals, reservations, events and outbox commit atomically. Composite SQL foreign keys bind source, tool version, financial rows and approval; receipts additionally require the same Task's exact version1 artifact/digest. Append-only tables and transition/version guards prevent rewrites. The Go validator/scanner also verifies full intent/digest/Unicode/type consistency before admission and reads. SQL insert guards are not a complete duplicate of every typed JSON-field rule: missing/corrupt serialized intent fields fail closed at the application boundary; further direct-SQL shape hardening remains under W01/W02.

These caps are not the complete storage quota, fair scheduler, provider billing, sandbox, production security or usability acceptance. The full original scope remains tracked in `docs/architecture/WORK_ORDERS.md`.
