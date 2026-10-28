<!-- SPDX-License-Identifier: Apache-2.0 -->
# Agent Platform Python SDK

API0.14 model metadata exposes `accounting_mode` (`metered` or
`operator_unmetered_local`) and input/output/request-time bounds. A local zero
API tariff is an operator declaration, not measured hardware cost or a settled
invoice. `CostReport.unknown_reservation_count` is an exact nonnegative decimal
string over the full task subtree, including zero-amount rows beyond the page.
For example, `unknown_microusd == "0"` can coexist with a positive unknown count.
Do not infer completed accounting from a zero sum or a page length. This SDK
preserves the count string and validates model mode/charge consistency.

## Explicit configuration plans (0.12)

`configuration_catalog()`, `configuration_destinations()`,
`configuration_resources()`, `configuration_grants()`, `configuration_plans()`,
`configuration_verifications()` and `configuration_audit()` inspect public
metadata. Lists accept keyword `cursor`/`limit`; verifications also accept
`plan_id`. Exact reads are `configuration_grant(id)`, `configuration_plan(id)`
and `configuration_verification(id)`.

Commands accept exported wire-shaped `TypedDict` inputs and an explicit keyword
`idempotency_key`: `create_configuration_grant`, `revoke_configuration_grant`,
`create_configuration_plan`, `validate_configuration_plan`,
`approve_configuration_plan`, `apply_configuration_plan` and
`start_configuration_verification`. Every command has its own key. Private host
profiles, endpoints, credential environments, keys, CA certificates and worker
permits are not public SDK inputs or exports. Configuration responses use strict
field allowlists, including nested objects; they do not retain unknown fields.

```python
catalog = client.configuration_catalog()
destinations = client.configuration_destinations()
# Select an exact approved destination. Inspect resources and required pages
# before supplying a known resource version. Zero means absent.
destination = next(d for d in destinations.data if d['id'] == approved_destination_id)
plan = client.create_configuration_plan({
    'grant_id': configuration_grant_id,  # required for delegated service/member
    'base_revision': catalog['revision'],  # keep the exact decimal string
    'ttl_seconds': 600,
    'operations': [{'id':'setup', 'destination_id':destination['id'],
                    'destination_version':destination['version'],
                    'expected_resource_version':known_resource_version,
                    'active':True}],
    'reason':'Set up the approved connection',
}, idempotency_key='setup-plan-unique')
validation = client.validate_configuration_plan(plan['id'], {
    'expected_version':plan['version'], 'plan_digest':plan['digest'],
}, idempotency_key='setup-validation-unique')
print(plan['id'], validation['valid'], validation['decision']['reason_codes'])
```

Validation has no external I/O. `valid=True` can still include a
`requires_approval` obligation. A current human owner separately approves the
exact digest and validation ID. Apply binds that validation and a reason and
saves settings once; it does not test connectivity. Verification separately
requires `expected_plan_version`, `plan_digest`, `operation_id`, canonical string
`max_charge_microusd`, `acknowledge_cost=True` and a reason. A model probe leaves
an unaccepted task candidate; provider usage does not settle unknown billing.
MCP discovery is explicitly unmetered and does not call tools. One verification
per plan/operation is allowed. Lost/unknown results never trigger a hidden POST
retry or replacement verification. Revocation preserves applied changes,
historical receipts and unknown liabilities; ancestor allocation counters never
reset through child grants.

A synchronous, typed client for public contract **0.14.0**. Python 3.10 or newer;
no third-party runtime dependencies. The client uses the standard library's
`urllib`, verifies HTTPS certificates, and does not follow redirects or retry
requests automatically.

This package is a client for the local platform under development. It does not
provide an agent runtime, a model provider, or a production-readiness guarantee.

## Install

From the repository root:

```sh
python3 -m pip install ./packages/python-sdk
```

For development without installation:

```sh
export PYTHONPATH="$PWD/packages/python-sdk/src"
```

## Submit a task

Obtain a platform token, organization ID, and published agent ID through the
server's authorized setup flow. Use HTTPS except for local development.
Environment variables below are example configuration conventions for this SDK.

```python
import os
from agent_platform import Client

client = Client(
    os.environ["AGENT_PLATFORM_URL"],  # e.g. http://127.0.0.1:8080
    token=os.environ["AGENT_PLATFORM_TOKEN"],
    organization_id=os.environ["AGENT_PLATFORM_ORGANIZATION_ID"],
)

task = client.create_task(
    goal="Summarize the supplied account information",
    agent_id=os.environ["AGENT_PLATFORM_AGENT_ID"],
    budget_microusd=1_000_000,  # exactly USD 1; do not use floats
    idempotency_key="account-42-summary-2026-10-23",
)
print(task["id"], task["status"])
```

The URL can be a server origin or an API root ending in `/api/v1`. A client is
bound to one organization. The server checks the token's membership and role;
the organization header is not authorization by itself.

## Methods

| Method | Result |
| --- | --- |
| `create_task(goal, agent_id, budget_microusd, *, idempotency_key, max_runs=None, execution_timeout_seconds=None)` | `Task` dictionary |
| `list_tasks(*, cursor=None, limit=None)` | `Page[Task]` |
| `get_task(task_id)` | `Task` dictionary |
| `cancel_task(task_id, *, idempotency_key)` | `Task` dictionary |
| `list_agents(*, cursor=None, limit=None)` | `Page[Agent]` |
| `list_models(*, cursor=None, limit=None)` | `Page[AvailableModel]` |
| `create_agent(name, instructions, *, idempotency_key, model=None, tools=None, budget_microusd=None, team=None)` | Draft `Agent` dictionary |
| `publish_agent(agent_id, *, expected_version, idempotency_key)` | Published `Agent` dictionary |
| `events(task_id, *, after=None, limit=None)` | `Page[Event]`, JSON polling |
| `get_task_event_snapshot(task_id)` | `TaskEventSnapshot` |
| `task_event_feed(task_id, *, cursor=None, limit=None)` | `TaskEventBatch` |
| `stream_task_events(task_id, *, cursor=None)` | Context manager of `Iterator[TaskEventStreamFrame]` |
| `artifacts(task_id, *, cursor=None, limit=None)` | `Page[Artifact]`, inline text artifacts |
| `runs(task_id, *, cursor=None, limit=None)` | `Page[Run]` |
| `operations(task_id, *, cursor=None, limit=None)` | `Page[Operation]` |
| `actions(task_id, *, cursor=None, limit=None)` | `Page[Action]` |
| `approvals(task_id, *, cursor=None, limit=None)` | `Page[Approval]` |
| `costs(task_id, *, limit=None, reservation_cursor=None, entry_cursor=None, incident_cursor=None, quarantine_cursor=None)` | `CostReport` dictionary |
| `candidates(task_id, *, cursor=None, limit=None)` | `Page[TaskCandidate]` |
| `decisions(task_id, *, cursor=None, limit=None)` | `Page[TaskDecision]` |
| `children(task_id, *, cursor=None, limit=None)` | `Page[Task]` |
| `review_task(task_id, *, expected_version, candidate_id, artifact_id, artifact_version, content_digest, decision, reason=None, idempotency_key)` | `Task` |
| `retry_task(task_id, *, expected_version, reason, acknowledge_new_cost, feedback=None, idempotency_key)` | `Task` |

Single-object responses unwrap the API's `data` envelope. Pages expose `.data`
and `.next_cursor`. Responses retain unknown fields for forward compatibility;
`TypedDict` definitions describe the documented fields without changing values.
Timestamps remain RFC3339 strings. Budgets range from 1 to 1,000,000,000,000
micro-USD, as specified by contract 0.6.0. The SDK does not locally enforce permission
policies or determine whether a result is accepted; those decisions belong to
the server.

### Create and publish an agent

```python
draft = client.create_agent(
    name="Fixture example",
    instructions="Exercise the development fixture end to end.",
    model="fixture:echo",  # deterministic test fixture, not an LLM
    tools=[],
    budget_microusd=10_000,
    idempotency_key="fixture-example-draft-1",
)
published = client.publish_agent(
    draft["id"],
    expected_version=draft["version"],
    idempotency_key="fixture-example-publish-1",
)
```

An unpublished draft cannot be executed. Publication includes the version you
inspected so a stale draft cannot silently be substituted. Configure a real
provider and choose its supported model explicitly before expecting inference.

### Pagination and event resumption

```python
cursor = None
while True:
    page = client.list_tasks(cursor=cursor, limit=50)
    for task in page.data:
        print(task["id"], task["status"])
    cursor = page.next_cursor
    if cursor is None:
        break

last_processed_sequence = "0"
page = client.events("task-id", after=last_processed_sequence, limit=100)
for event in page.data:
    print(event["type"], event["legacy_data_json"] if event["payload_version"] == 0 else event["data"])
    last_processed_sequence = event["sequence"]
```

Task and agent list cursors are opaque strings. Event `after` is a canonical decimal string
sequence, not a list cursor. Persist it **after processing** an event; sequences
are increasing within an organization and may have gaps. This method reads JSON history; separate streaming APIs are described below. Limits are between 1 and 100.

### Execution and accounting reads (0.3)

The four execution lists use opaque `cursor` values and the same `Page` shape
as `list_tasks`, ordered by `(created_at, id)` descending. Their default page
size is 50, maximum 100. Each method is scoped to the requested task.

```python
runs = client.runs(task["id"], limit=50)
report = client.costs(task["id"], limit=50)
print(report["spent_microusd"], report["unknown_microusd"], report["financial_hold"])
if "next_entry_cursor" in report:
    next_page = client.costs(task["id"], entry_cursor=report["next_entry_cursor"], limit=50)
    more_entries = next_page["entries"]
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

Contract 0.3 is a breaking wire update: monetary facts/counters are canonical
`Money = str` values, including task spent/reserved amounts, reservation
amount/actual, signed ledger deltas, incident/quarantine amounts and report
totals. Admission budgets remain bounded integers. There is no numeric fallback
for earlier contracts. `root_id`, `depth` and `financial_hold` are required Task
fields; `parent_id` and pinned model connection fields may be absent.

```python
from agent_platform import parse_micro_usd
exact = parse_micro_usd("9223372036854775807")  # exact Python int
correction = parse_micro_usd("-1", signed=True)
```

The helper rejects noncanonical strings, JSON numbers, out-of-range int64,
negative unsigned amounts, `-0`, leading zeros, fractions and whitespace.
Never convert financial facts to float. `CostReport` additionally contains
`quarantines`, `pending_quarantine_count`, `incomplete`, and optional
`next_quarantine_cursor`; pass `quarantine_cursor` to page that collection.
An incomplete report has pending facts that are not booked totals: do not
present it as final spend or sum quarantine amounts into the totals yourself.
The SDK validates all report money fields, collection shapes, summary types,
booleans and cursors, while preserving extension fields. It does not validate
every nonfinancial nested field or replace server authorization and accounting.

`CostQuarantine`, `AccountingOutcome`, `CostEvidence`, `CostAdjustment`, `Money`
and `AvailableModel` are public wire types alongside the execution types.
The latter billing types do not expose a billing submission method.
`list_models` returns configured organization profiles with explicit price
assumptions and limits; it never returns credential references or endpoints.

### Review a candidate and explicitly request another attempt (0.4)

Fetch a fresh task, follow candidate pages to its exact `candidate_id`, and fetch
the referenced artifact ID/version. Compare the SHA-256 digest of the exact
UTF-8 content that the reviewer sees before submitting a decision. Never choose
the newest arbitrary artifact or infer missing current-run metadata.

```python
import hashlib

current = client.get_task(task["id"])
# candidate and artifact below are the exact referenced objects fetched above.
assert candidate["id"] == current["candidate_id"]
assert candidate["run_id"] == current["current_run_id"]
assert artifact["id"] == candidate["artifact_id"]
assert artifact["version"] == candidate["artifact_version"]
assert hashlib.sha256(artifact["content"].encode("utf-8")).hexdigest() == candidate["content_digest"]
blocked = client.review_task(
    current["id"], expected_version=current["version"],
    candidate_id=candidate["id"], artifact_id=artifact["id"],
    artifact_version=artifact["version"], content_digest=candidate["content_digest"],
    decision="request_changes", reason="Support the conclusions with sources.",
    idempotency_key="account-42-review-1",
)
# Only after explicit authorization for another potentially paid attempt:
queued = client.retry_task(
    blocked["id"], expected_version=blocked["version"],
    reason="Revise the draft", feedback="Add sources for both conclusions.",
    acknowledge_new_cost=True, idempotency_key="account-42-attempt-2",
)
```

`accept`, `reject`, and `request_changes` are authorized user decisions, not
proof of human presence or factual truth. Reject ends the task; request_changes
only records feedback and pauses it. `retry_task` creates a new root run with
pinned feedback and may incur new costs; it is distinct from replaying an HTTP
request with the original idempotency key. Existing spend and unknown reserves
remain. The server rechecks access, current candidate/version, descendant state,
financial hold, budget, deadline, and lifetime attempt limits. Managed children
cannot be retried directly; review can conflict after their parent consumes them.

`reason` is required and nonblank for reject/request_changes and retry (4096 UTF-8
bytes); retry `feedback` is required after request_changes (8192 bytes). The SDK
cannot infer that state from a retry payload. All text excludes NUL. Creation
accepts `max_runs` 1–10 (default 3), `execution_timeout_seconds` 1–2592000
(default 86400). New execution fields on Task and `Run.attempt` remain optional
for durable 0.3 idempotency-cache responses; refresh before acting when absent.

### Bounded teams

Create an agent with `model="team:coordinator"`, `tools=[]`, and `team` containing
`pattern` (`sequence`, `parallel`, or `delegate_verify`), pinned `members`
(`id`, `agent_id`, `agent_version`, optional local `task`), `join_policy`
(`all` or `allow_partial`), `max_depth` 1–16 and `max_children` 1–32.
Optional `max_parallel` is 1–32 and cannot exceed member count. Delegate/verify
requires exactly two distinct members and uses concurrency 1; parallel defaults
to member count. Member tasks must be nonblank when present, at most 4096 UTF-8
bytes. The server validates published versions and a finite materialized tree
with at most 128 tasks / 384 attempts. Agent verification does not automatically
establish truth or accept the root result. Use `children` to traverse the tree,
and `candidates` / `decisions` for immutable result and decision history.

## Retries and errors

Every mutation requires a caller-provided idempotency key, up to 128 printable
ASCII characters without spaces. Persist a key with the business operation. Reuse that same
key and the same input after a lost response; use a new key for different work.
The server scopes keys to organization, principal, method, and route. Reusing a
key with different input produces a conflict. A retry still checks current
permissions.

```python
from agent_platform import APIError, ProtocolError, TransportError

try:
    task = client.get_task("task-id")
except APIError as error:
    print(error.status_code, error.code, error.request_id, error.retryable)
except TransportError:
    # A lost response to a mutation may mean that the work was already accepted.
    # Inspect state or retry the original mutation with its ORIGINAL key.
    raise
except ProtocolError:
    # Unexpected JSON or response shape; do not assume a mutation was rolled back.
    raise
```

`retryable` is server advice, not an automatic retry policy. HTTP errors from a
proxy without a valid envelope preserve status and `X-Request-ID` but do not
expose the proxy's response body in the exception. Successful JSON responses
are capped at 16 MiB by the client. The server has its own request and artifact
size limits. The SDK makes no exactly-once guarantee for external actions.

## Tests and example

From the repository root:

```sh
python3 -m unittest discover -s tests/sdk -v
python3 examples/python/fixture_task.py --help
```

Tests use a temporary HTTP server bound to `127.0.0.1`; no external service or
credentials are required. They exercise authentication headers, payloads,
cursors, structured errors, a lost response, and refusal to follow redirects.
The fixture example requires a running platform server and an authorized token.

Licensed under Apache-2.0; see `LICENSE`.


### Editable materials and exact publication (0.5)

`materials(task_id, cursor=..., limit=...)` lists logical draft heads.
`get_material(task_id, material_id)` gets the latest metadata;
`material_revisions(task_id, material_id, cursor=..., limit=...)` lists immutable
revision metadata. `get_material_version(task_id, material_id, version)` returns
`{revision, content}` for the exact stored version. `get_artifact(task_id,
artifact_id, version=1)` reads an exact immutable execution artifact. Artifact
lists are now paginated (default 20); do not assume the current candidate appears
on the first page.

```python
material = client.create_material(
    task_id, "result.md", "text/markdown", "Draft conclusion",
    base_candidate_id=current_candidate_id, idempotency_key="document-1",
)
material = client.revise_material(
    task_id, material["id"], "Revised conclusion",
    expected_version=material["head_version"], reason="Clarify the evidence",
    idempotency_key="document-1-edit-2",
)
current = client.get_task(task_id)
proposed = client.publish_material(
    task_id, material["id"], expected_task_version=current["version"],
    expected_material_version=material["head_version"],
    content_digest=material["head_content_digest"],
    parent_candidate_id=current["candidate_id"], idempotency_key="document-1-publish-2",
)
```

Saving is separate from publication and acceptance. Draft content can be empty
or unfinished JSON. Publication requires nonblank content and valid JSON when
its media type is `application/json`; the server checks the stored version.
A publication proposes a candidate while the task is waiting for a decision.
It neither starts a model attempt nor accepts the result, and preserves cost
holds. Team review retains its original immutable basis; edited prose is not a
new authoritative team manifest. `TaskCandidate` exposes optional
`parent_candidate_id`, `review_basis_candidate_id`, `material_id`,
`material_version`, and `published_by` metadata.

A revision conflict raises `APIError(code="conflict")`; the client does not merge
or retry. Keep your buffer, read the latest version, combine changes explicitly,
and submit with `expected_version=latest["head_version"]` and
`merge_base_version=the_version_you_originally_read`. Supply a new idempotency
key for that distinct merged operation. Reuse the old key only when retrying the
same unchanged operation after an uncertain response. Content is bounded to
262,144 UTF-8 bytes, names to 120, reasons to 4,096, and revisions to 1..128.
No secret or server implementation is included in this SDK.

`client.get_current_identity()` reads `/me`: the authenticated principal, organization, role and effective permissions. It returns no credential. Re-read it after reconnecting before associating a local draft with an identity; it does not grant future access to a document.

### Exact tool actions (0.6)

`tools()` lists safe metadata; `tool_executions(task_id)` lists execution metadata
without argument bodies. `get_tool_execution(task_id, execution_id)` returns the
exact string and server-computed `available_commands`.

`prepare_tool_execution`, `decide_tool_execution`, `dispatch_tool_execution`, and
`reconcile_tool_execution` all require an explicit `idempotency_key`. Prepare
binds candidate/artifact/digest, tool version, raw `arguments_json` object string,
a stable `occurrence_id`, and `expires_in_seconds` (1..86400). Raw JSON numeric
lexemes and whitespace are preserved. Use the returned `view["task"]["version"]`;
reservation can increment the task version more than once.

A decision is `approve` or `deny`, with a reason. Approval alone never executes.
Dispatch is separate and requires `acknowledge_cost=True` plus exact reviewed
task, execution, approval versions and intent hash. No call is automatically
retried. An unknown external outcome must not be blindly resent. Administrative
reconciliation records an operator assertion, not factual truth, human presence,
financial finality or acceptance of the task candidate.

### Native model proposals and continuation (0.7)

Create an agent with `tools=[registered_tool_id]`; the server returns immutable
`agent["model_tools"]` definitions. Clients cannot supply or change those pins.
`model_proposals(task_id)` lists metadata; `get_model_proposal(task_id, proposal_id)`
returns exact arguments. Prepare it with `model_proposal_id=proposal["id"]` and
`occurrence_id="model:" + proposal["id"]`, copying its candidate, artifact,
version, digest, tool version and unchanged `arguments_json`. Preparation does
not approve or send the action.

After inspecting every result in the current batch, `continue_model` takes all
exact `ModelToolResultBinding` entries, the current task/candidate/artifact
snapshot, a reason, `acknowledge_cost=True`, and an explicit idempotency key.
Succeeded/rejected entries require the exact receipt ID and content digest;
denied/expired/cancelled entries carry no invented receipt. Unknown or incomplete
effects block continuation. The new model turn uses the same Run and a separate
reserved action. Previous cost holds remain; final prose still requires review.
The SDK never prepares a proposal or continues automatically.

See `examples/python/model_tools.py` for separate preparation and continuation
helpers. Calling the latter explicitly acknowledges a potentially new model
charge; inspect the exact results before doing so.

### Task automation and progress signals (0.8)

`automation(task_id)` only reads. `create_automation(..., idempotency_key=...)`
explicitly delegates bounded task work; `revoke_automation` needs the exact
`grant_id`, `expected_version`, nonblank `reason` and an idempotency key.
`progress_signals(task_id, cursor=..., limit=...)` paginates conservative stop
evidence. The SDK never creates a grant on initialization or retries a mutation.

Create keyword arguments are `expected_task_version`, `mode`, `tool_pins`,
`max_model_turns`, `max_tool_executions`, `max_charge_microusd`,
`expires_in_seconds`, `acknowledge_cost=True`, `reason` and `idempotency_key`.
Select 1..16 exact pins (`tool_id`, `tool_version`, `tool_profile_digest`) from
the task's pinned agent version. Turn and execution ceilings are 1..31 and
1..32; the charge ceiling is a **canonical decimal string** from `"1"` to
`"1000000000000"` micro-USD. Expiry is 1..86400 seconds, also bounded by the
task deadline. Python numeric or boolean money is rejected before HTTP.

`read_only` can execute only explicitly administrator-opted-in read tools
without a distinct approver requirement. `prepare_and_continue` prepares
proposals, but each action still needs separate authorized approval **and
explicit dispatch**. The next model step follows registered known outcomes.
Unknown outcomes stop automatic work and are never resent automatically. A final
candidate always needs a separate user acceptance decision.

Limits and usage are cumulative for the whole task, across Retry and replacement
grants. A new grant does not add its ceilings to previous ceilings or clear
usage. Allocated charge is an admission estimate, not a final invoice.
`available_commands` reflects current authority; the server rechecks it on every
mutation. Creation/revocation currently requires administrator or owner authority.

Grants survive browser logout. Explicitly revoke a grant to stop future automatic
work; expiry, deprovisioning or current authority loss also stop it. Revocation
does not undo sent actions or release unknown liabilities, and child tasks do not
inherit grants. Repetition evidence calls for review; it does not establish truth
or prove that a model hallucinated.


## Organization members (0.9)

```python
caps = client.membership_capabilities()
page = client.list_members(limit=50)  # active and inactive; opaque cursor
member = client.create_member("Local colleague", "viewer", reason="Provision local access",
                              idempotency_key="member-create-1")
member = client.update_member(member["principal_id"], expected_version=member["version"],
    display_name=member["display_name"], role="member", active=True,
    reason="Approved role change", idempotency_key="member-role-1")
history = client.list_membership_audit(principal_id=member["principal_id"], limit=20)
```

Owner/admin permission and each target's fresh `available_commands` and
`assignable_roles` govern administration. `get_member` reads the exact current
snapshot. Self-management is unavailable; an admin cannot manage an owner.
The server protects the last active owner across concurrent requests. A stale CAS returns `conflict`; a no-op full update returns `invalid_transition`; inspect current state rather than
silently retrying with a newer version.

Creation allocates a new local principal. It accepts no email or existing
identity identifier, returns no secret, and sends no invitation. A trusted
operator connects login separately. Names are local to the organization.

`revoke_member_credentials(principal_id, expected_version=..., reason=...,
idempotency_key=...)` invalidates prior credentials in this organization only.
It keeps the role and membership unchanged; independent durable task grants
continue. Disable the member or revoke the exact task grant to stop that
separate authority. A role edit keeps login usable with current rights; any
active-status change requires fresh credentials and cannot resurrect old task
authority. Already admitted or sent actions cannot be undone, and unknown money
holds are preserved. Audit entries contain safe before/after access state and
reasons, never credentials. These routes do not implement SCIM or automatic SSO
onboarding.

## Organization security (0.10)

`get_login_policy()` and `update_login_policy(...)` read/change the organization
policy with an exact version, explicit `service_access_enabled` boolean and reason.
Changing either login mode or issuer requires a current human owner browser
session at the configured trusted issuer; bearer owners may change application
access and the TTL while retaining mixed login. Readiness comes from the server.

Use `list_service_accounts`, `get_service_account`, `create_service_account`,
`update_service_account` (full version/name/role/active/reason),
`list_service_keys`, `get_service_key`, `revoke_service_key`, and
`list_security_audit(principal_id=...)`. Mutations require explicit idempotency
keys and never automatically retry. Service roles/immutable key ceilings are
`member` or `viewer`; services cannot administer people or accept human results.

There is no HTTP/SDK secret issuance method. An authorized host operator runs
`platform issue-service-token --organization ORG --principal SERVICE --role-ceiling viewer --ttl 1h --reason 'Integration setup' --out /secure/new-service-key.json`.
Only the new private file receives the secret; metadata APIs never return it.
The key is bound to its organization. Expiry/revocation prevents new requests,
while accepted tasks remain durable. Disable the service account or organization
application access to revoke accepted execution consent. Re-enabling never
revives old credentials/work. TTL policy caps new issuance, not existing keys.
Unknown effects and accounting holds remain unchanged.


### Versioned live events (0.11)

Protected requests send and verify `X-Platform-API-Version: 0.14.0`. Missing or
incompatible response versions raise `ProtocolError`, including after a mutation;
no request is automatically replayed. Public `/api/v1/compatibility` allows version
discovery without credentials.

```python
snapshot = client.get_task_event_snapshot("task-id")
cursor = snapshot["cursor"]
with client.stream_task_events("task-id", cursor=cursor) as frames:
    for frame in frames:
        if frame["kind"] == "error":
            raise RuntimeError(frame["error"]["code"])
        if frame["kind"] == "reset_required":
            # Explain the missing history, then explicitly request a new snapshot.
            break
        for event in frame["batch"]["events"]:
            process_event(event)
        cursor = frame["batch"]["cursor"]  # Persist after the entire batch.
batch = client.task_event_feed("task-id", cursor=cursor, limit=100)
```

The snapshot and cursor share a committed database boundary. Cursors are opaque
organization/task scoped read coordinates, never credentials. An empty input
starts from the replay origin. A history reset returns neither events nor a usable
cursor; do not silently skip it. Sequences are canonical decimal **strings** up to
positive int64 and may contain gaps. Convert to Python int only for local arithmetic.

Version1 events are typed from the Apache2 public payload registry and strictly
validated, including unknown fields/kinds, variants, constants and precise string
money. Version0 retains exact `legacy_data_json` as an opaque string and has no
`data`; it must not be promoted to a current typed fact.

Use the context manager even when exiting early: it closes the HTTP response.
There is one connection and no hidden reconnect; the caller chooses when to
reconnect using the last fully processed cursor. `timeout` bounds connection and
idle reads. The server ends a connection within20seconds and sends heartbeat
comments. The strict platform SSE subset permits UTF8/comments/event/id/multiline
data and CR/LF/CRLF, bounded64KiB frames. Duplicate or unknown fields, mismatched
IDs, malformed JSON or incomplete EOF raise `ProtocolError`. Error/reset frames
never contain an ID. No subscription triggers a command, inference or tool action.

## Product support and onboarding catalogue (0.13)

`client.capabilities()` returns a typed `ProductCapabilities` observation for the
current organization and credential. It reads `/api/v1/capabilities` without
query parameters, a request body, or an idempotency key. `read_only` is always
true and `assessment` is `support_and_role_observation`.

Operations describe supported surfaces, permission alternatives, runtime guards,
effects, cost obligations, profiles and six onboarding paths. These facts never
grant permission or prove that a later operation can execute. Each operation
still checks current authorization and its resource state. Configuration grants
remain separate from role permissions. Commands are inert argv templates; this
SDK never executes them. Documentation references are repository-relative.

The client rejects incompatible catalogue/API versions, another organization,
unknown identity kinds and enums, duplicate IDs/bindings, malformed or oversized
records, unsafe documentation references and command templates. With a fixed
credential, a principal observed through identity or catalogue introspection is
also checked on later catalogue reads. No principal is inferred from the token.
