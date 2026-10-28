# Public contract 0.14.0

Status: revision0.14 adds explicit local-model accounting, safe request bounds
and a full-subtree unknown-reservation count; see [LOCAL-MODELS.md](LOCAL-MODELS.md).
The current-core checkpoint (internal historical reference omitted) passed11/11 groups;
full release acceptance remains incomplete. Revision0.13 added the product capability catalogue across HTTP, CLI,
SDKs, Workspace and the private MCP adapter. Its current verification is
separate from the saved main-profile0.12 deployment with SQL020.
CORE-0.12.md (internal historical reference omitted) and the
deployment evidence (internal historical reference omitted)
remain historical evidence for that revision. No new migration is required.
Full enterprise/provider release readiness remains unaccepted.

`GET /capabilities` describes implemented operations, support profiles, effective
role observations and six onboarding personas. It grants no authority and does
not inspect tenant connection secrets or call providers. See
[CAPABILITIES.md](CAPABILITIES.md) and the [user guide](../docs/CAPABILITIES.md).

Owner: integrator. Consumers: Go server, web, Python/TypeScript SDKs and the bounded private MCP adapter. UTF-8 JSON, RFC3339 UTC timestamps, opaque string IDs, integer micro-USD. No floating-point money. Factual monetary amounts and signed ledger deltas are canonical decimal strings preserving the full int64 range; existing policy budgets remain bounded JSON integers (at most1e12). Tool ceilings and0.8 automation ceilings/allocations use bounded canonical decimal strings. Unknown optional response fields may be ignored; invalid or unknown mutation input fields are rejected. Revision0.3 changes factual money from JSON numbers to strings and requires matching SDKs.

## HTTP

Base `/api/v1`. `Authorization: Bearer <opaque token>` identifies a principal. `X-Organization-ID` selects an organization in which that principal has an active membership. Never accept a user identity from another header. Development bootstrap is an explicit setup command, not an unauthenticated endpoint. Reads and effects recheck membership/roles. Missing/revoked credentials → 401, missing permission → 403, missing/cross-tenant resource → 404.

Revision0.14 requires exactly one `X-Platform-API-Version: 0.14.0` header on every authenticated API request. A missing, ambiguous or unsupported version returns409 `incompatible_client` before command decoding or idempotency replay. `GET /api/v1/compatibility` provides public discovery; matching server/UI/SDK versions must be deployed together. Responses expose the current revision. [EVENTS.md](EVENTS.md) specifies precise compatibility, snapshot, replay and streaming behavior.

Configured browser login additionally supports Authorization Code + PKCE, an opaque Secure/HttpOnly session cookie and CSRF-protected mutations. Bearer and session credentials in the same request are rejected. Organization membership is still mandatory. `GET /auth/config` discovers login availability; the `/auth/` browser flow is described in identity-login.md. Organization policy can require this exact configured issuer for human access, with separate bounded service identities; see [SECURITY.md](SECURITY.md). Broader corporate identity lifecycle and provider compatibility remain incomplete.

Success `{ "data": ... }`; lists `{ "data": [...], "next_cursor": "..." }` (next_cursor absent when exhausted). Errors `{ "error": { "code": "...", "message": "...", "request_id": "...", "retryable": false } }`. No raw SQL/internal error messages. Errors: invalid_request, unauthenticated, forbidden, not_found, conflict, incompatible_client, budget_exceeded, invalid_transition, stale_lease, unavailable, internal_error.

Mutations require `Idempotency-Key` (1–128 safe printable ASCII characters). Scope = organization + principal + method + canonical route + key. Canonical decoded payload hash detects changed input → 409 conflict. Identical request returns the original result. Keys and results are durable; permissions rechecked before replay. Requests capped at 1 MiB; goals 16 KiB; instructions 32 KiB; names 120 bytes; list default 50/max100 (artifacts default20/max100). Mutations that race serialize against the underlying record. No success acknowledgment before commit.

## Agent definitions

`POST /agents`: `{name,instructions,model?,tools?,budget_microusd?,team?}` → Agent (201). model defaults to explicit `fixture:echo` in development, tools defaults `[]`; fixture models always labeled. A bounded team uses `team:coordinator` and an immutable `TeamDefinition`; its members reference published agent versions. `GET /agents` → list. `POST /agents/{id}/publish`: `{expected_version}` → immutable published Agent snapshot; 409 on stale version. Fields: id, organization_id, name, instructions, model, tools:string[], budget_microusd:int, version:int, published:bool, created_at, team?. Drafts cannot be executed. Later updates create new revisions; active runs keep a pinned snapshot and current authorization.

`GET /models?cursor=...&limit=...` lists administrator-installed active models with safe limits and declared pricing assumptions. It does not expose endpoint credentials or secret references. Nonfixture agents must select an installed model. Revision0.7 allows up to16 distinct installed tool IDs on a model agent and returns readonly `model_tools` version/profile/schema pins; fixture/coordinator agents cannot declare tools. [MODEL-TOOLS.md](MODEL-TOOLS.md) specifies native calls, intermediate proposals and durable continuation.

## Tasks

`POST /tasks`: `{goal,agent_id,budget_microusd,max_runs?,execution_timeout_seconds?}` → Task (201), pinned to a published agent version. `max_runs` is 1–10, default 3; execution timeout is 1–2592000 seconds, default 86400. These lifetime limits cannot reset on retry. `GET /tasks` → list ordered by created_at,id descending; `cursor` is opaque. `GET /tasks/{id}` → Task. `POST /tasks/{id}/cancel`: `{}` → Task, idempotent terminal handling and cancellation of nonterminal descendants. Once a managed child's result is consumed, decide or cancel through its parent/root.

Task fields: id, organization_id, owner_id, agent_id, agent_version, goal, status, budget_microusd, reserved_microusd:string, spent_microusd:string, version, created_at, updated_at, waiting_reason?, result?, root_id, parent_id?, depth, financial_hold, model_connection_id?, model_connection_version?, current_run_id?, attempt_count?, max_runs?, execution_deadline?, candidate_id?, accepted_decision_id?. Root lineage, goal, agent and model version pins are immutable; root depth is zero. Optional lifetime response fields preserve old durable idempotency responses; fresh reads provide them. Dependency tasks have no current run before activation. The public creation API creates root tasks; the coordinator materializes their closed dependency trees.

States: queued → running|blocked|failed. running → succeeded|failed|waiting_input|waiting_approval|blocked. queued/running/waiting_input/waiting_approval/blocked → cancelled. A waiting model/team/material candidate can become succeeded or failed by an exact authorized decision; request_changes blocks the Task and closes the old Run. Explicit root retry may create a new Run only from the documented blocked states. Terminal succeeded/failed/cancelled cannot resume. Generic approval/resume mutations remain unimplemented. Success records its verification/decision method; it does not prove factual truth.

Revision 0.4 adds `POST /tasks/{id}/review`, `POST /tasks/{id}/retry` and `GET /tasks/{id}/candidates`, `/decisions`, `/children`. Exact snapshot binding, cost acknowledgment, inherited receipts and team limits are specified in [COLLABORATION.md](COLLABORATION.md). No decision settles or releases money.

`GET /tasks/{id}/events?after=<decimal-string>&limit=<n>` reads the immutable full history. Revision0.11 encodes positive `sequence` as an exact decimal string. New events use registered typed `payload_version:1` and `data`; historical version0 exposes unchanged stored JSONB text as opaque `legacy_data_json`, with no typed data field. The versioned snapshot/feed/SSE contract and logical replay window are specified in [EVENTS.md](EVENTS.md). Sequence gaps remain normal.

Event sequence allocation is serialized until commit within an organization, so a visible later cursor cannot overtake an earlier uncommitted event. Initial pending-task admission limit is 1000 per organization, including waiting and blocked tasks; reaching it returns retryable unavailable/503. This is bounded admission, not the finished fair scheduler or API rate-limiting system.

`GET /tasks/{id}/artifacts?cursor=...&limit=...` → paginated Artifact list (default20/max100). `GET /tasks/{id}/artifacts/{artifact_id}?version=1` returns one exact immutable version (default1). Fields: id, organization_id, task_id, name, media_type, content:text, version:int, created_at, provenance:object. Initial inline artifacts are text capped at 256 KiB; object storage is required before full large-file acceptance.

## Current identity and collaborative materials (0.5)

`GET /me` returns current `principal_id`, `organization_id`, `role`, effective `permissions` and optional `principal_kind` (`human` or `service`; included in fresh0.10 reads). It uses the ordinary current credential/membership check; tokens, credential hashes, sessions and secrets are never returned. It accepts no query parameters. This is introspection, not delegated authority.

Eight implemented material/exact-artifact routes add append-only drafts, optimistic conflicts, explicit manual merge provenance and publication as a new candidate. [MATERIALS.md](MATERIALS.md) specifies shapes, bounds, permissions, publication guards and unchanged financial obligations. Editing a draft does not create a Run or change the Task version/result; publication requires a separate exact snapshot command, followed by a separate result decision.

New material and artifact endpoints reject unknown, repeated and malformed query fields. Queryless routes reject query fields. List routes accept only `cursor`/`limit`; exact artifact reads accept only a positive `version` (an explicitly empty version is invalid). The material list and revision list contain metadata; exact content is requested by version.

## External actions and exact approvals (0.6)

Seven implemented routes add safe tool discovery and durable external occurrences: [TOOL-EXECUTIONS.md](TOOL-EXECUTIONS.md). Each occurrence pins its source Task/Run/Operation/candidate/artifact, exact argument string and SHA-256, immutable tool/endpoint/account/price profile, maximum exposure, expiry and authenticated creator. Preparation reserves funds but performs no external call. Approval is a separate authorized principal decision; dispatch additionally requires explicit cost acknowledgment and a fresh snapshot. It does not establish human presence.

`ToolExecution` and immutable effect receipts are separate from financial `Action`/Reservation. A confirmed invoice cannot establish business success; a tool response or operator assertion cannot settle its bill. Unknown effects retain liabilities and cannot be automatically resent. Explicit operator resolution records evidence without another call or financial settlement. Contradictory late observations append bounded conflict evidence instead of replacing the first receipt.

Tools are installed from trusted-host private configuration; endpoints, credentials and CA bundles are not exposed by discovery. User/API/MCP actors can explicitly request these actions. Revision0.7 adds native model `tool_calls`, immutable agent tool pins and explicit same-Run continuation; automatic execution/continuation policies and OS sandbox integration remain incomplete. No arbitrary stdio/executable is admitted by this action profile.

## Native model tools and continuation (0.7)

`GET /tasks/{id}/model-tool-proposals` lists metadata; `GET /tasks/{id}/model-tool-proposals/{proposal_id}` reads exact arguments. A `model_tools` candidate cannot be accepted as a final answer, material-published or consumed as a team result. Model requests do not themselves authorize a tool action. Existing prepare/approve/dispatch commands require the exact proposal and permanent occurrence identity.

`POST /tasks/{id}/continue-model` requires all exact terminal proposal outcomes, current versions, cost acknowledgement and reason. It queues one new Operation in the same Run and preserves old unknown liabilities. Unknown effects prohibit continuation. The persisted transcript contains exact assistant calls and quoted role=tool observations. Lifetime counters apply across Runs/retries and do not reset on a new lease. Full shapes, bounds and separate policy limits: [MODEL-TOOLS.md](MODEL-TOOLS.md).

## Internal persistence contracts

Every object has organization_id and scoped composite foreign keys. Run pins agent version and initiating principal. Operation has durable status, lease_owner, lease_until, fencing_token. Lease claim/renew/complete use server database time, transactional CAS and monotonically increasing fencing token. Old workers cannot commit after lease loss. External side effects require Action(intent hash, stable idempotency key, pending|sent|confirmed|unknown|rejected); unknown cannot be silently retried.

Reserve integer upper-bound cost atomically across every ancestor and organization before dispatch. Settlement is idempotent by immutable provider evidence reference and payload; changed facts conflict and corrections require an expected revision. Unknown actual cost retains reserve. Verified actuals exceeding the reserved upper bound are recorded in full within the supported factual range, with an incident and financial hold. Tree budget and organization cap are both authoritative. See [COSTS.md](COSTS.md) for trusted accounting and range limitations.

Schema migrations are versioned; event insertion is atomic with state update. Database restore and process-kill recovery must be tested independently before W02 acceptance. Authentication, budget, actions and audit are shared across API/UI/SDK/MCP.

## Inspection and shared definitions

`GET /tasks/{id}/runs`, `/operations`, `/actions`, `/approvals` return typed lists with independent `cursor` and `limit`. `GET /tasks/{id}/costs` returns a `CostReport` object with whole-subtree summaries and four independently paginated collections (`reservation_cursor`, `entry_cursor`, `incident_cursor`, `quarantine_cursor`). `incomplete` and `pending_quarantine_count` explicitly signal accepted facts that could not yet be projected into bounded counters. Ordinary organization membership grants the documented role permissions; no user role can submit trusted accounting evidence. See [EXECUTION.md](EXECUTION.md) for bindings and state guards.

Canonical Go DTOs generate `domain.schema.json`, `openapi.json`, `state-machines.json`, `permissions.json` and `errors.json`. Only implemented HTTP routes are advertised in OpenAPI (82 method/path pairs in0.13). Definitions for future approval mutations do not make those APIs available. The private MCP adapter invokes these implemented routes with current authorization; its bounded transport and incomplete OAuth profile are documented in [MCP-ADAPTER.md](../docs/MCP-ADAPTER.md). JSON polling is a separate API contract.

Revision0.8 adds four implemented task-automation/signal routes, explicit bounded delegation, cumulative usage, exact job grant bindings and deterministic automatic-stop signals: [AUTOMATION.md](AUTOMATION.md). Final output still needs a separate decision. Logout does not revoke a durable grant. Existing explicit workflows and unresolved financial obligations are preserved.

JSON Schema `maxLength` counts Unicode code points. The `x-max-utf8-bytes` annotation documents stricter server byte limits; callers must also perform semantic validation (nonblank, valid UTF-8 and no NUL). Team duplicate IDs and delegate/verifier count are semantic checks in `domain.ValidateTeam`. CostAdjustment additionally requires a nonblank `evidence.reason`; ordinary CostEvidence can omit it.

Revision0.9 adds seven implemented organization membership/capability/audit routes.
See [MEMBERSHIPS.md](MEMBERSHIPS.md) for provisioning, current permissions,
credential-only revocation, execution epochs and organization isolation.

Revision0.10 adds ten implemented organization login-policy, service-account, key-metadata/revocation and security-audit routes. Exact policy epochs, admission-only key TTL/revocation, trusted-host issuance/recovery and remaining scope are specified in [SECURITY.md](SECURITY.md).

Revision0.12 adds17 configuration-plane routes for exact approved destinations, attenuated capability envelopes, immutable plans, owner approvals, atomic application and separately acknowledged connection verification. See [CONFIGURATION.md](CONFIGURATION.md) and the [operator/user guide](../docs/CONFIGURATION.md). The local verification/deployment record is CORE-0.12.md (internal historical reference omitted); full work-order acceptance remains tracked in PROGRESS.md.
