<!-- SPDX-License-Identifier: Apache-2.0 -->
# Agent Platform TypeScript SDK

API0.14 model metadata exposes `accounting_mode` (`metered` or
`operator_unmetered_local`) and input/output/request-time bounds. A local zero
API tariff is an operator declaration, not measured hardware cost or a settled
invoice. `CostReport.unknown_reservation_count` is an exact nonnegative decimal
string over the full task subtree, including zero-amount rows beyond the page.
For example, `unknown_microusd === "0"` can coexist with a positive unknown count.
Preserve the string or use `BigInt` rather than `Number`. This SDK validates
the count and model mode/charge consistency; it never settles costs from transport.

## Explicit configuration plans (0.12)

`configurationCatalog()`, `configurationDestinations()`, `configurationResources()`,
`configurationGrants()`, `configurationPlans()`, `configurationVerifications()`
and `configurationAudit()` inspect public metadata; lists accept `ListOptions`
and return ordinary pages. Verifications additionally accept `planId`.
`configurationGrant(id)`, `configurationPlan(id)` and
`configurationVerification(id)` read exact records.

The seven commands are `createConfigurationGrant`, `revokeConfigurationGrant`,
`createConfigurationPlan`, `validateConfigurationPlan`,
`approveConfigurationPlan`, `applyConfigurationPlan` and
`startConfigurationVerification`. Every command needs its own explicit
`{idempotencyKey}`. Their exported request DTOs use **wire snake_case**, matching
Python and CLI JSON. Private host profiles, endpoints, credential environments,
keys, CA certificates and worker permits are not SDK inputs or exports.
Configuration responses reject unknown fields, including nested fields, rather
than forwarding a private host profile through a permissive extension object.

```ts
const catalog = await client.configurationCatalog();
const destinations = await client.configurationDestinations();
// Choose an exact approved destination; inspect resources and all required
// pages before supplying the expected resource version. Zero means absent.
const destination = destinations.data.find(d => d.id === approvedDestinationId)!;
const plan = await client.createConfigurationPlan({
  grant_id: configurationGrantId, // required for a delegated service/member
  base_revision: catalog.revision, // exact decimal string, never Number(...)
  ttl_seconds: 600,
  operations: [{id:'setup', destination_id:destination.id,
    destination_version:destination.version,
    expected_resource_version:knownResourceVersion, active:true}],
  reason:'Set up the approved connection',
}, {idempotencyKey:'setup-plan-unique'});
const validation = await client.validateConfigurationPlan(plan.id, {
  expected_version:plan.version, plan_digest:plan.digest,
}, {idempotencyKey:'setup-validation-unique'});
console.log(plan.id, validation.valid, validation.decision.reason_codes);
```

Validation performs no network probe. A valid plan can still have a
`requires_approval` obligation: a current human owner must explicitly approve
the exact digest and validation ID before application. Apply requires those
bindings and a reason, changes settings once, and does not start a probe.
Verification is another explicit command with `expected_plan_version`,
`plan_digest`, `operation_id`, canonical string `max_charge_microusd`,
`acknowledge_cost:true` and a reason. Model verification uses ordinary task/cost
records; its candidate is unaccepted and usage is not a provider bill. MCP
verification discovers a registered unmetered catalog and never calls tools.
One verification per plan/operation is allowed. A lost reply or unknown outcome
does not authorize a replacement probe; the SDK never retries POSTs. Revoke
does not undo applied settings or release unknown financial exposure. Shared
grant counters remain cumulative across descendants.

A typed ESM client for public contract **0.14.0**, using native `fetch` and
`AbortController` on Node.js 24 or newer. No runtime dependencies. HTTPS uses
the runtime's certificate validation. Redirects and automatic retries are
disabled.

This is a client for the local platform under development, not an agent runtime
or a production-readiness guarantee. Both this package and the Python SDK use
the same API contract.

## Build and install locally

From this package directory:

```sh
npm install
npm run build
npm test
```

`pnpm install --frozen-lockfile`, `pnpm build`, and `pnpm test` are also supported;
the committed pnpm lockfile pins the compiler. TypeScript is a development-only
dependency. Install the built local package into your application with:

```sh
npm install /absolute/path/to/agent-platform/packages/typescript-sdk
```

## Submit a task

```ts
import { Client } from "@agent-platform/sdk";

const client = new Client({
  baseUrl: "http://127.0.0.1:8080",
  token: process.env.AGENT_PLATFORM_TOKEN!,
  organizationId: process.env.AGENT_PLATFORM_ORGANIZATION_ID!,
  timeoutMs: 30_000,
});

const task = await client.createTask(
  {
    goal: "Summarize the supplied account information",
    agentId: process.env.AGENT_PLATFORM_AGENT_ID!,
    budgetMicrousd: 1_000_000, // exactly USD 1
  },
  { idempotencyKey: "account-42-summary-2026-10-23" },
);
console.log(task.id, task.status);
```

Obtain credentials, organization membership, and a published agent through the
server's authorized setup flow. The organization header is a selector, not an
authorization mechanism. Use HTTPS outside local development. A token callback
can supply a refreshed token for every request:

```ts
const client = new Client({
  baseUrl,
  organizationId,
  token: async () => tokenStore.currentAccessToken(),
});
```

The callback acquires a platform-accepted token; the SDK does not implement an
OAuth login flow or retry a request after a 401. Keep long-lived service tokens
out of frontend bundles.

## API

Methods and input options use camelCase, except the configuration request DTOs below, which deliberately use exact wire snake_case. Responses retain the API's snake_case
field names and unknown response fields. Single responses unwrap `data`; list
responses expose `{data, next_cursor?}`. Timestamps remain RFC3339 strings.

| Method | Result |
| --- | --- |
| `createTask({goal, agentId, budgetMicrousd, maxRuns?, executionTimeoutSeconds?}, {idempotencyKey, signal?})` | `Promise<Task>` |
| `listTasks({cursor?, limit?, signal?}?)` | `Promise<Page<Task>>` |
| `getTask(id, {signal?}?)` | `Promise<Task>` |
| `cancelTask(id, {idempotencyKey, signal?})` | `Promise<Task>` |
| `listAgents({cursor?, limit?, signal?}?)` | `Promise<Page<Agent>>` |
| `models({cursor?, limit?, signal?}?)` | `Promise<Page<AvailableModel>>` |
| `createAgent({name, instructions, model?, tools?, budgetMicrousd?, team?}, {idempotencyKey, signal?})` | `Promise<Agent>` |
| `publishAgent(id, {expectedVersion, idempotencyKey, signal?})` | `Promise<Agent>` |
| `events(taskId, {after?, limit?, signal?}?)` | `Promise<Page<Event>>` |
| `getTaskEventSnapshot(taskId, {signal?}?)` | `Promise<TaskEventSnapshot>` |
| `taskEventFeed(taskId, {cursor?, limit?, signal?}?)` | `Promise<TaskEventBatch>` |
| `streamTaskEvents(taskId, {cursor?, signal?, idleTimeoutMs?}?)` | `AsyncGenerator<TaskEventStreamFrame>` |
| `artifacts(taskId, {cursor?, limit?, signal?}?)` | `Promise<Page<Artifact>>` |
| `runs(taskId, {cursor?, limit?, signal?}?)` | `Promise<Page<Run>>` |
| `operations(taskId, {cursor?, limit?, signal?}?)` | `Promise<Page<Operation>>` |
| `actions(taskId, {cursor?, limit?, signal?}?)` | `Promise<Page<Action>>` |
| `approvals(taskId, {cursor?, limit?, signal?}?)` | `Promise<Page<Approval>>` |
| `costs(taskId, {limit?, reservationCursor?, entryCursor?, incidentCursor?, quarantineCursor?, signal?}?)` | `Promise<CostReport>` |
| `candidates(taskId, {cursor?, limit?, signal?}?)` | `Promise<Page<TaskCandidate>>` |
| `decisions(taskId, {cursor?, limit?, signal?}?)` | `Promise<Page<TaskDecision>>` |
| `children(taskId, {cursor?, limit?, signal?}?)` | `Promise<Page<Task>>` |
| `reviewTask(taskId, ReviewTaskInput, {idempotencyKey, signal?})` | `Promise<Task>` |
| `retryTask(taskId, RetryTaskInput, {idempotencyKey, signal?})` | `Promise<Task>` |

Budgets are integer micro-USD between 1 and 1,000,000,000,000 in contract 0.6.0.
Integer inputs must be safe JavaScript integers;
use no floating-point currency. Unsafe integer responses are rejected instead
of silently accepting a rounded budget or sequence. Limits range from 1 to 100.

### Drafts and publication

```ts
const draft = await client.createAgent(
  {
    name: "Fixture example",
    instructions: "Exercise the development fixture end to end.",
    model: "fixture:echo", // deterministic test fixture, not an LLM
    tools: [],
    budgetMicrousd: 10_000,
  },
  { idempotencyKey: "fixture-example-draft-1" },
);
const published = await client.publishAgent(draft.id, {
  expectedVersion: draft.version,
  idempotencyKey: "fixture-example-publish-1",
});
```

Draft agents cannot execute tasks. A stale `expectedVersion` conflicts instead
of silently publishing a changed draft. A real model must be configured and
explicitly selected before expecting inference.

### Pagination and events

```ts
let cursor: string | undefined;
do {
  const page = await client.listTasks({ ...(cursor ? {cursor} : {}), limit: 50 });
  for (const task of page.data) console.log(task.id, task.status);
  cursor = page.next_cursor;
} while (cursor !== undefined);

let lastProcessedSequence = "0";
const page = await client.events("task-id", {after: lastProcessedSequence, limit: 100});
for (const event of page.data) {
  console.log(event.type, event.payload_version === 0 ? event.legacy_data_json : event.data);
  lastProcessedSequence = event.sequence;
}
```

List cursors are opaque strings. Event `after` is a canonical decimal string sequence, persisted
after processing an event. Sequences increase within the organization; gaps are
allowed. `events` remains a JSON history read. Use the separate snapshot/feed/stream APIs below.

### Execution and accounting reads (0.3)

The four execution lists use opaque `cursor` values and the same `Page` shape
as `listTasks`, ordered by `(created_at, id)` descending. Their default page
size is 50, maximum 100. Each method is scoped to the requested task.

```ts
const runs = await client.runs(task.id, { limit: 50 });
const report = await client.costs(task.id, { limit: 50 });
console.log(report.spent_microusd, report.unknown_microusd, report.financial_hold);
if (report.next_entry_cursor !== undefined) {
  const nextPage = await client.costs(task.id, { entryCursor: report.next_entry_cursor, limit: 50 });
  const moreEntries = nextPage.entries;
}
```

`CostReport` includes the selected task's entire subtree. `reservations`,
`entries`, `incidents`, and `quarantines` have independent opaque cursors:
`next_reservation_cursor`, `next_entry_cursor`, `next_incident_cursor`, and `next_quarantine_cursor`.
An omitted next cursor means that collection is exhausted. Omitting an input
cursor starts that collection at its first page; when fetching more entries,
consume only that collection rather than appending repeated reservation pages.
Each response is one consistent server snapshot; separate page requests are
separate snapshots and can observe subsequent updates.

`budget_microusd`, `reserved_microusd`, `spent_microusd`, `unknown_microusd`, and
`financial_hold` are server totals for the subtree, not page totals. Never sum
those totals across pages or parent and child reports. Ledger deltas are signed
decimal strings. Preserve `synthetic` when displaying fixture costs; a synthetic entry
is not an external provider bill. Missing `actual_microusd` means no actual was
returned, not a zero charge. The SDK exposes **no accounting mutations** and
cannot authorize a refund, release a hold, or decide an approval.

Contract 0.3 is a breaking wire update. Financial facts/counters are canonical
`Money = string` values: task spent/reserved, reservation amount/actual, signed
ledger deltas, incident/quarantine amounts and report totals. Admission budgets
remain bounded numbers. Earlier numeric facts are not a compatible fallback.
Required Task fields include `root_id`, `depth`, `financial_hold`; parent and
pinned model connection fields may be absent.

```ts
import { parseMicroUSD } from "@agent-platform/sdk";
const exact = parseMicroUSD("9223372036854775807"); // 9223372036854775807n
const correction = parseMicroUSD("-1", true);       // -1n, signed delta
```

This helper returns `bigint` without a `Number` conversion. It rejects numeric
inputs, noncanonical strings, int64 overflow, negative unsigned values, `-0`,
leading zeros, fractions and whitespace. Preserve the original decimal string
when serializing JSON; native JSON cannot encode bigint directly.

`CostReport` additionally contains `quarantines`, `pending_quarantine_count`,
`incomplete`, and optional `next_quarantine_cursor`; pass `quarantineCursor`
when paging that collection. An incomplete report contains pending facts that
are not booked spend: do not add quarantine amounts to its totals yourself.
Runtime checks validate all report money fields, collection shapes, summary
numbers, booleans and cursors; they do not replace server validation of every
other nested field. Extension fields are preserved.

`CostQuarantine`, `AccountingOutcome`, `CostEvidence`, `CostAdjustment`, `Money`
and `AvailableModel` are exported wire types alongside the execution types.
The billing types do not add a billing submission method. `models()` returns
configured organization profiles and explicit limits/price assumptions, without
endpoint or credential material. This SDK does not implement browser SSO.

### Candidate decisions and new attempts (0.4)

Refresh the task, page candidates to its exact `candidate_id`, and fetch the
referenced artifact ID/version. Compute SHA-256 over the exact UTF-8 content
displayed to the reviewer and compare `content_digest`. Bind the command to
that snapshot; do not silently substitute the latest artifact during review.

```ts
const current = await client.getTask(task.id);
// candidate and artifact are the exact referenced, inspected objects.
const blocked = await client.reviewTask(current.id, {
  expectedVersion: current.version,
  candidateId: candidate.id, artifactId: artifact.id,
  artifactVersion: artifact.version, contentDigest: candidate.content_digest,
  decision: "request_changes", reason: "Support both conclusions with sources.",
}, {idempotencyKey: "account-42-review-1"});
// Obtain explicit authorization for another potentially paid execution first.
const queued = await client.retryTask(blocked.id, {
  expectedVersion: blocked.version, reason: "Revise the draft",
  feedback: "Add sources for both conclusions.", acknowledgeNewCost: true,
}, {idempotencyKey: "account-42-attempt-2"});
```

Review supports `accept`, `reject`, and `request_changes`. A decision records
authorized intent, not factual truth or verified human presence. Reject ends
the task. Request_changes pauses it without launching work; a root `retryTask`
creates a new run with pinned feedback, preserving previous spend and unknown
reserves. It is distinct from replaying a transport request using its original
idempotency key. The server rechecks current version/candidate, authorization,
descendant state, budget, financial hold, deadline, and lifetime attempt limit.
Managed children cannot be retried directly; their review can conflict after
parent consumption. `reason` is nonblank for reject/request_changes/retry and
bounded to 4096 UTF-8 bytes; feedback is required after request_changes and
bounded to 8192 bytes. Retry payload alone cannot establish that task state.
The explicit `acknowledgeNewCost: true` is mandatory at runtime and in types.

CreateTask accepts `maxRuns` 1–10 (default 3), `executionTimeoutSeconds` 1–2592000
(default 86400). Task execution metadata and `Run.attempt` remain optional in
response types to support durable cached 0.3 replies. Fetch fresh metadata
before decisions instead of guessing limits. All new text excludes NUL and
invalid surrogate sequences.

### Bounded team presets

`createAgent` accepts `team: TeamDefinition` with the **wire field names**
`pattern`, `members`, `join_policy`, `max_depth`, `max_children`, and optional
`max_parallel`. Choose `model: "team:coordinator"` and `tools: []`. Members pin
`agent_id` and positive `agent_version`; distinct `id` and optional nonblank
`task` (4096 UTF-8 bytes) identify their role. Patterns are sequence, parallel,
and delegate_verify; delegate_verify requires two members and concurrency 1.
Depth is 1–16, child/member/concurrency limits 1–32; concurrency cannot exceed
members. Parallel defaults to member count. Publication and execution validate
the finite plan (at most 128 tasks / 384 attempts) and published pinned versions.
Use `children` for tree traversal and `candidates` / `decisions` for immutable
history. A verifier agent's output is not proof of truth or human acceptance.

## Cancellation, timeouts, and errors

`timeoutMs` bounds token acquisition, the request, and body reading. Every
method accepts an `AbortSignal`:

```ts
const controller = new AbortController();
const pending = client.getTask("task-id", {signal: controller.signal});
controller.abort();
await pending; // rejects with TransportError(reason: "aborted")
```

Aborting a request or reaching its timeout does not cancel a task already
accepted by the server. Use `cancelTask` for that explicit mutation. A lost
response may leave the mutation's result unknown.

```ts
import { APIError, ProtocolError, TransportError } from "@agent-platform/sdk";

try {
  await client.getTask("task-id");
} catch (error) {
  if (error instanceof APIError) {
    console.error(error.statusCode, error.code, error.requestId, error.retryable);
  } else if (error instanceof TransportError) {
    console.error(error.reason); // network, timeout, or aborted
  } else if (error instanceof ProtocolError) {
    console.error("Unexpected server response");
  } else {
    throw error;
  }
}
```

Every mutation requires an idempotency key of up to 128 printable ASCII
characters without spaces. Persist a key with its operation, and reuse the same key and input
after a lost response. Use a new key for different work. The server scopes keys
to principal, organization, method, and route; changed input conflicts and
permissions are rechecked on replay. `retryable` is advice, not automatic retry.

Redirect refusal is reported as a `TransportError` by native fetch. Invalid
success envelopes raise `ProtocolError`. Non-JSON HTTP errors preserve status
and `X-Request-ID` without exposing the raw error body. Responses are capped at
16 MiB. No exactly-once guarantee for external actions is implied.

## Tests and example

The HTTP tests use `node:test` and a temporary server bound only to
`127.0.0.1`. They cover request contracts, typed errors, token refresh callbacks,
integer validation, redirect refusal, lost responses, caller cancellation, and
timeouts while receiving a response or waiting for a token. No external service
or real credential is required.

After building, run `node examples/typescript/fixture_task.ts --help` from the
repository root. The runnable example requires an authorized server and uses
an explicitly labeled development fixture.

License: Apache-2.0, see `LICENSE`.


### Editable materials and exact publication (0.5)

`materials(taskId, options?)` lists draft heads; `getMaterial(taskId, materialId)`
gets the latest head metadata. `materialRevisions(taskId, materialId, options?)`
returns paginated immutable metadata, and `getMaterialVersion(taskId, materialId,
version)` returns `{revision, content}`. `getArtifact(taskId, artifactId,
{version: 1})` reads an exact immutable execution artifact. Artifact lists now
support opaque cursors and limits (server default 20), so the first page need
not contain the current candidate.

```ts
let material = await client.createMaterial(taskId, {
  name: "result.md", mediaType: "text/markdown", content: "Draft conclusion",
  baseCandidateId: currentCandidateId,
}, {idempotencyKey: "document-1"});
material = await client.reviseMaterial(taskId, material.id, {
  expectedVersion: material.head_version, content: "Revised conclusion",
  reason: "Clarify the evidence",
}, {idempotencyKey: "document-1-edit-2"});
const current = await client.getTask(taskId);
const proposed = await client.publishMaterial(taskId, material.id, {
  expectedTaskVersion: current.version,
  expectedMaterialVersion: material.head_version,
  contentDigest: material.head_content_digest,
  parentCandidateId: current.candidate_id!,
}, {idempotencyKey: "document-1-publish-2"});
```

Saving, publication and acceptance are separate operations. Empty or unfinished
JSON drafts are allowed; publication requires nonblank stored content and valid
JSON for `application/json`. The server checks the exact version. Publication
proposes a candidate only while the task awaits a decision; it does not start a
model attempt, accept the result, clear reservations or establish factual truth.
An edited team result retains its original immutable review basis. Candidate
metadata includes optional `parent_candidate_id`, `review_basis_candidate_id`,
`material_id`, `material_version` and `published_by`.

A 409 raises `APIError` without merging or retrying. Keep your buffer, read the
latest head and version, then explicitly combine changes and call `reviseMaterial`
with the latest `expectedVersion` and the original `mergeBaseVersion`. This is a
new operation requiring a new idempotency key. An uncertain retry of the same
unchanged operation must reuse its previous key. Content is capped at 262,144
UTF-8 bytes, names at 120, reasons at 4,096 and revision numbers at 1..128.
All methods accept request cancellation; mutations require an explicit key.

`await client.getCurrentIdentity()` reads `/me`: the authenticated principal, organization, role and effective permissions. It returns no credential. Re-read it after reconnecting before associating a local draft with an identity; it does not grant future access to a document.

### Exact tool actions (0.6)

`tools()` discovers safe metadata; `toolExecutions(taskId)` lists metadata and
`getToolExecution(taskId, executionId)` fetches exact argument bytes and current
server-computed `available_commands`. Lists preserve opaque cursors.

`prepareToolExecution`, `decideToolExecution`, `dispatchToolExecution`, and
`reconcileToolExecution` require an explicit idempotency key. Prepare binds the
current candidate/artifact/digest, tool version, raw `argumentsJson` object string,
stable `occurrenceId`, and expiry (1..86400 seconds). Large numeric lexemes are
never reserialized. The returned `view.task.version` is authoritative; reservation
can increment it more than once.

A decision uses `approve` or `deny` and a reason. Approval never sends the tool
request. Dispatch is a separate call with `acknowledgeCost: true` and the exact
task, execution, approval versions and intent hash from the reviewed view.
An unknown outcome must not be blindly resent. Reconciliation records an
authorized operator assertion; it neither proves truth nor settles a bill.
The SDK does not infer human presence and never automatically approves, dispatches,
retries, or reconciles. Tool outcomes do not accept the task candidate.

### Native model proposals and continuation (0.7)

Create an agent with registered `tools` IDs. Its optional read-only `model_tools`
contains server-pinned public definitions. `modelProposals(taskId)` lists metadata;
`getModelProposal(taskId, proposalId)` returns exact arguments. Prepare with
`modelProposalId: proposal.id`, `occurrenceId: "model:" + proposal.id` and the
unchanged source/artifact/digest, tool/version and `argumentsJson` from that exact
proposal. No approval or external call follows automatically.

`continueModel` requires all terminal `ModelToolResultBinding` entries for the
current batch, its exact task/candidate/artifact snapshot, a reason, an explicit
idempotency key, and `acknowledgeCost: true`. Result bindings retain wire names:
`proposal_id`, `execution_id`, `execution_version`, `status`, plus exact
`receipt_id`/`content_digest` for succeeded or rejected effects. Denied, expired
and cancelled effects carry no fabricated receipt. Unknown and incomplete
effects cannot continue. The next model turn has a new reserved action in the
same Run; old cost holds and final result review remain separate.

See `examples/typescript/model_tools.ts` for separate helpers. The caller chooses
each invocation after inspecting the exact proposal/results; there is no
automatic continuation, retry, approval, or dispatch.

### Task automation and progress signals (0.8)

`automation(taskId)` is a read. `createAutomation(taskId, input, {idempotencyKey})`
explicitly delegates bounded work; `revokeAutomation` stops further automatic
work using the exact grant ID and version. `progressSignals` paginates the
append-only reasons for conservative stops. Opening a client does not create a
grant, and the SDK never retries these mutations automatically.

Create input uses `expectedTaskVersion`, `mode`, `toolPins`, `maxModelTurns`,
`maxToolExecutions`, `maxChargeMicrousd`, `expiresInSeconds`, `acknowledgeCost:true`
and a nonblank `reason`. Pins use exact wire fields `tool_id`, `tool_version`,
`tool_profile_digest` from the task's pinned agent version; select an explicit
subset of 1..16 tools. Limits are 1..31 automatic model turns, 1..32 tool
executions and a **canonical decimal string** charge ceiling of 1..1e12
micro-USD. Expiry is 1..86400 seconds, also bounded by the task deadline.

`read_only` can execute only administrator-opted-in read tools that do not
require a distinct approver. `prepare_and_continue` prepares the actions but
requires separate authorized approval **and explicit dispatch** of each one;
the model then continues after registered known observations. An unknown outcome
stops automatic work and is never resent automatically. The final model
candidate still needs an explicit user decision; no grant accepts it.

Usage and limits are cumulative over the task's lifetime. Retry or a replacement
grant never resets used turns, executions or allocated charge. Allocated charge
is an admission estimate, not a confirmed invoice. `available_commands` describes
current authority and can change before a mutation. Grant creation/revocation
currently requires organization administrator or owner authority.

A grant survives browser logout. Explicitly revoke it to stop further automatic
work; expiry, deprovisioning and loss of required current authority also stop it.
Revocation does not undo sent actions or release unknown liabilities. Grants are
not automatically inherited by child tasks. Repetition signals are reasons for
review, not a truth detector or proof of hallucination.


## Organization members (0.9)

```ts
const caps = await client.membershipCapabilities();
const page = await client.listMembers({limit:50}); // active and inactive
let member = await client.createMember({displayName:"Local colleague",role:"viewer",
  reason:"Provision local access"},{idempotencyKey:"member-create-1"});
member = await client.updateMember(member.principal_id,{expectedVersion:member.version,
  displayName:member.display_name,role:"member",active:true,reason:"Approved role change"},
  {idempotencyKey:"member-role-1"});
const history = await client.listMembershipAudit({principalId:member.principal_id,limit:20});
```

Use `getMember` to obtain a fresh snapshot and current `available_commands` and
`assignable_roles`. Owner/admin permission is checked by the server on every
request. Self-management is unavailable; an admin cannot manage an owner. The
server protects the last active owner across concurrent changes. `active` is a
required boolean, including false. A stale CAS returns `conflict`; a no-op update returns `invalid_transition`; the SDK
never silently substitutes a new version or retries a mutation.

Creation provisions a new local principal: no email or existing identity is
accepted, no invitation is sent and no credential is returned. A trusted
operator connects login separately. Display names are organization-local.

`revokeMemberCredentials(principalId,{expectedVersion,reason},{idempotencyKey})`
invalidates prior credentials in this organization only. It keeps role and
membership unchanged; independent durable task grants continue. Disable the
member or revoke the exact task grant to stop that separate authority. A role
edit keeps credentials usable with current rights; any active-status change
requires fresh credentials and cannot restore old task authority. Already
admitted or sent actions cannot be undone; unknown money holds persist. Audit
responses contain safe local before/after state and reasons, never secrets.
These routes do not imply SCIM or automatic SSO onboarding.

## Organization security (0.10)

Use `getLoginPolicy` / `updateLoginPolicy` with full `UpdateLoginPolicyInput`,
exact `expectedVersion`, explicit `serviceAccessEnabled`, TTL and reason.
Mode/issuer transitions require a current human-owner browser session at the
configured trusted issuer; bearer owners can retain mixed login and change only
application access/TTL. The server supplies readiness and available commands.

Service APIs: `listServiceAccounts`, `getServiceAccount`, `createServiceAccount`,
`updateServiceAccount`, `listServiceKeys`, `getServiceKey`, `revokeServiceKey`,
`listSecurityAudit({principalId})`. Mutations require `{idempotencyKey}` and never
automatically retry. Full updates require explicit `active` and exact version.
Service roles and immutable key ceilings are only `member` / `viewer`.

Credentials are issued only on the trusted host:
`platform issue-service-token --organization ORG --principal SERVICE --role-ceiling viewer --ttl 1h --reason 'Integration setup' --out /secure/new-service-key.json`.
No SDK/HTTP method mints or returns the secret. Metadata contains opaque IDs,
role ceiling, expiry, status and commands. A key works only in its organization.
Key revocation/expiry blocks new requests; accepted tasks continue. Deactivate
the account or disable organization application access to revoke accepted
execution consent; re-enabling does not revive old keys or work. New TTL caps
apply to new issuance. Unknown-effect holds and cost counters are preserved.


### Versioned live events (0.11)

Every protected request sends `X-Platform-API-Version: 0.14.0` and validates the
same response header. A missing or different version is a `ProtocolError`; no
command is automatically replayed. The public `/api/v1/compatibility` endpoint
allows discovery without credentials. Package versions do not negotiate the wire format.

```ts
const snapshot = await client.getTaskEventSnapshot("task-id", {signal});
let cursor = snapshot.cursor;
for await (const frame of client.streamTaskEvents("task-id", {cursor, signal})) {
  if (frame.kind === "error") throw new Error(frame.error.code);
  if (frame.kind === "reset_required") {
    // Explain the missing history and explicitly obtain a new snapshot.
    break;
  }
  for (const event of frame.batch.events) await processEvent(event);
  cursor = frame.batch.cursor; // Save only after the whole batch is processed.
}
const batch = await client.taskEventFeed("task-id", {cursor, limit: 100, signal});
```

Snapshot task state and cursor share a committed database boundary. `cursor` and
`replay_cursor` are opaque, organization/task scoped read coordinates, not
credentials. Empty input starts from the replay origin; unavailable history returns
`reset_required` with no cursor or events. Do not silently skip that reset. Decimal
sequence strings preserve the full positive int64 range; gaps are permitted.

Version1 events form a discriminated `TypedEvent` union generated from the Apache2
public payload registry; unknown kinds, fields, versions or invalid primitives fail
closed. Version0 events carry **only** opaque `legacy_data_json`; do not parse it
through JavaScript Number or treat it as a current typed payload.

`streamTaskEvents` opens one connection and never reconnects or sends commands.
The server bounds connections to20seconds and emits heartbeat comments. Reconnect
explicitly from the last fully processed cursor. Fetch streaming supports bearer
headers without putting secrets in URLs. A separate connection timeout and
`idleTimeoutMs` (default30seconds) bound waiting. Breaking the loop or aborting
closes the body reader. The platform accepts a strict bounded subset of SSE:
UTF8, comments, one event/id, multiline data, CR/LF/CRLF, maximum64KiB per frame;
unknown fields, duplicate fields, mismatched IDs and incomplete EOF are protocol
errors. Error/reset frames have no ID. This is not MCP transport or a webhook.

## Product support and onboarding catalogue (0.13)

`await client.capabilities({ signal })` returns a typed `ProductCapabilities`
observation using the current organization and credential. The request has no
query parameters, body or idempotency key. `read_only` is always true and
`assessment` is `support_and_role_observation`.

Operations describe supported surfaces, permission alternatives, runtime guards,
effects and cost obligations. Profiles and six onboarding paths describe bounded
support. These facts never grant execution permission. Each resource action
still checks current authorization; configuration delegation remains separate.
Commands are inert argv templates and are never executed. Documentation paths
refer to local repository documentation.

The client checks version, organization, known identity kinds/enums, unique IDs
and bindings, bounded shape, local documentation paths and safe command
templates. For a fixed token it also checks a previously observed principal.
Token callbacks may intentionally refresh the active principal, so those clients
validate each response's current organization, kind, role and permission scope
without retaining a principal from a previous credential.
