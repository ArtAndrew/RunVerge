# Native model tools and durable continuation — 0.7

This contract extends the bounded non-streaming function-tool wire profile. It
does not establish compatibility with every model provider, autonomous approval
policy, or a universal exactly-once remote effect. Native tool requests are
untrusted output. Model text is never parsed as a command.

## Agent definitions and tool pins

`CreateAgent.tools` selects at most16 distinct installed tool IDs for a real
model agent. Fixture and coordinator agents cannot declare tools. The server
resolves each active version once and returns readonly `Agent.model_tools`:
`tool_id`, `tool_version`, `tool_profile_digest`, deterministic ASCII `name`,
`description`, `input_schema_json`. A published version keeps those pins; it
does not silently follow the newest installation. Reactivation or replacement
of a retired tool requires an explicit new definition/version.

The wire alias is `t_` plus the first32 lowercase SHA-256 hex characters of
`organization_id + "/" + tool_id + "/" + decimal_version`. Endpoint, credential
reference and CA certificate are never part of the prompt or public pin.
Descriptions, schemas and tool results remain untrusted content, not authority.

Model requests include only these pinned function tools. Each native response
may request up to8 function calls, preserving their order and exact decoded
argument string bytes. Call IDs and aliases use `[A-Za-z0-9_-]{1,64}`. IDs are
unique throughout the stored conversation. Multiple calls to one alias with
different IDs are allowed. Duplicate keys, malformed Unicode/JSON, unknown
aliases, invalid arguments and oversized batches are rejected as a whole.
There is no partial execution of the valid subset.

## Intermediate candidate and proposals

A native batch creates an immutable `model_tools` candidate whose artifact is
`{text,tool_calls}` with exact argument strings. The task waits with
`model_tools_pending`. This is a request to use tools, not a final answer:

- It cannot be accepted as the final task result, published through a material
  revision or selected as a team's completed member output.
- It can be rejected, returned for changes or cancelled using normal current
  authorization and version checks.
- It creates no tool action, approval, reserve or network request by itself.

`GET /tasks/{id}/model-tool-proposals?cursor=...&limit=...` lists metadata;
default20/max100, `arguments_json` omitted. Exact bytes are available through
`GET /tasks/{id}/model-tool-proposals/{proposal_id}`. These reads use current
tenant membership and task access, not a previously cached identity.

Each proposal binds organization/Task/Run/Operation, candidate/artifact/version/
digest, provider call ID, pinned tool/profile/alias and argument digest. Optional
`execution_id` and `execution_status` describe its separately prepared action.

## Prepare, approve and dispatch

The existing [tool execution contract](TOOL-EXECUTIONS.md) remains authoritative.
For a native proposal, `PrepareToolExecution.model_proposal_id` is required and
`occurrence_id` must equal `"model:" + proposal_id`. Candidate, tool version,
profile and exact argument bytes must match the saved proposal. Each proposal
can bind at most one execution forever; changing an idempotency key or occurrence
cannot create another physical attempt for that proposal.

Preparation reserves budget; approval does not send; dispatch is separate and
requires current exact versions and cost acknowledgement. Up to8 calls in a
batch are processed through the current one-open-action-per-task gateway. This
profile serializes actions within one task; separate tasks may run concurrently.

Unknown effects stop the task for reconciliation and never trigger an automatic
resend. Confirming a bill cannot resolve an unknown effect. A team retains an
unresolved child rather than consuming its tool request as a final answer.

## Continue after all outcomes

`POST /tasks/{id}/continue-model` requires an idempotency key and:

```json
{
  "expected_task_version": 10,
  "candidate_id": "cand_exact",
  "artifact_id": "art_exact",
  "artifact_version": 1,
  "content_digest": "<64 lowercase hex>",
  "results": [
    {
      "proposal_id": "mtp_exact",
      "execution_id": "tex_exact",
      "execution_version": 4,
      "status": "succeeded",
      "receipt_id": "tor_exact",
      "content_digest": "<exact result artifact digest>"
    }
  ],
  "acknowledge_cost": true,
  "reason": "Use the recorded tool observations for the next model step"
}
```

The placeholder strings above illustrate the shape, not valid executable input.
Results must include every proposal exactly once. `succeeded`/`rejected` require
the exact immutable receipt and result digest. `denied`/`expired`/`cancelled`
have no receipt fields; their saved reason is quoted as a non-executed outcome.
Known-unsent native jobs stopped by kernel policy use `cancelled`, not an
invented external rejection receipt. `unknown` and unfinished jobs cannot
continue. Receipt methods, including `operator_assertion`, are retained so that
an assertion does not become verified evidence merely by entering model context.

The server checks current owner/caller authority, profile activation, deadline,
all expected versions, whole-tree state, exact next wire size, lifetime limits
and remaining shared budget. Permission is `tasks.continue_own` or
`tasks.continue_any`. The command queues a new Operation in the **same Run**;
Run/attempt counters are not reset or consumed as a user retry. A source
candidate can produce only one continuation. Commit acknowledgement loss and a
fresh idempotency key cannot bypass that uniqueness.

The queued worker independently reserves the next model charge and rechecks
current authority before admission. Existing sent/unknown model reservations
remain counted. Queueing is not a bill, settlement, acceptance, or proof of
network dispatch. This revision requires an explicit continuation command;
automatic continuation policies are still part of the unfinished full product.

## Transcript, bounds and recovery

The initial system instructions and complete user/Run input are saved once.
Each continuation appends the exact assistant batch and ordered `role=tool`
observations. Tool content never becomes a system message. The transcript is
append-only through separate model-turn records with SHA-256 and exact source
links. Worker lease claims, restarts and admission reconstruct it from durable
state; worker-supplied replacement history or schema pins fail validation.

There are32 model turns per task and128 per root over its lifetime, across user
retries as well as continuations. A claimed operation does not consume another
turn when its lease is renewed or reclaimed. This bounds calls; it is not a
semantic hallucination detector or evidence that an agent made useful progress.
Input/output byte limits, tree deadline and budget may stop work sooner.

Forward migrations013/014 preserve applied001–012. Historical roots above the
new turn policy retain their full counters/history and deny additional turns;
they are never clamped or reset to pass an upgrade. Internal transcripts allow
up to2MiB to preserve the quoted representation of a legacy900KB Run input.
The model wire request remains separately bounded to its installed profile and
at most1MiB; preserving data does not authorize an oversized new call.

New MCP mappings are `platform_model_proposals_list`,
`platform_model_proposals_get`, and opt-in `platform_model_continue`, through
the same REST checks. Catalog totals:25 readonly defaults,14 named opt-in
mutations,39 method/path pairs. SDKs do not retry effects or continuations
automatically. Earlier schemas and reports remain historical evidence.
