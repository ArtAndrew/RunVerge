# Task automation — contract 0.8

This is explicit durable delegation for a pinned native-model task, not authority
inferred from model text. The normal task budget, current organization policy,
exact tool intent, dispatch permit and observed result remain mandatory. No
automatic final-answer acceptance or financial settlement is introduced.

## Commands and discovery

| Method and task-relative path | Input / result |
|---|---|
| GET `/automation` | `TaskAutomationState`: latest grant if any, lifetime usage, latest20 signals, current `available_commands` |
| POST `/automation` | `CreateTaskAutomation` → state,201; ordinary durable idempotency required |
| POST `/automation/revoke` | `RevokeTaskAutomation` → state,200; exact grant ID/version and reason |
| GET `/progress-signals` | Immutable signal list; cursor/limit, default20/max100 |

Queryless routes reject query parameters. Mutation bodies reject unknown fields,
duplicate fields, null, invalid Unicode and noncanonical money strings. Monetary
ceilings and allocated amounts here are exact decimal strings in micro-USD,
bounded by1e12. This extends the bounded policy representation; it does not
change existing integer task-budget inputs or full-int64 factual accounting.

Grant creation requires the current `tasks.automate_any` permission plus current
global continuation, prepare, dispatch and approval permissions. Initially these
belong to admin/owner. A member cannot gain approval permission by creating a
grant. Task owner, issuer, organization, pinned agent/model and tool profiles are
rechecked. No capability is inherited by a newly created child Task.

Creation binds the current Task version, owner, immutable agent/model versions,
and1–16 unique exact tool ID/version/profile-digest pins from that agent. The
Task must have a real registered native-model profile and tools, remain
nonterminal/unblocked without a financial hold, and have time remaining. A
fixture/coordinator is not an eligible agent. A current active grant must first
be revoked; an expired grant may be closed when issuing a new one.

`TaskAutomationState.available_commands` contains `create` or `revoke` where the
current credential can attempt that command. Exact versions, state, money and
tool-specific eligibility are still checked at the mutation boundary. Read-only
introspection never changes a grant or starts a provider call.

## Two execution modes

`read_only`: the trusted operator must have installed every permitted tool with
effect=`read`, `automation_allowed=true`, and no distinct-approver requirement.
The coordinator checks the WHOLE proposed batch against these pins before its
first automatic preparation. A mixed, retired, duplicate or unlisted batch stops
automation without executing its safe subset. An eligible call is separately
prepared, approved with method=`task_automation_grant`, and queued. This is not
recorded as a human decision.

`prepare_and_continue`: the coordinator prepares a pinned proposal and reserves
its ceiling. Each tool still requires the ordinary separately authorized
approval AND explicit dispatch command. The grant does not auto-approve writes
or destructive actions and never satisfies a distinct-approver requirement.

Only exact registered `succeeded`/`rejected` tool receipts for EVERY proposal may
automatically queue the next model Operation in the SAME Run. Unknown outcomes,
operator assertions, denial, expiry and known-unsent cancellation require
attention. Manual continuation retains its0.7 semantics and exact snapshots;
an operator assertion is quoted as such and never promoted to proof.

Final model text remains an unverified candidate. The grant completes with
`final_candidate_requires_review`; accepting the candidate is a separate action.
Stopping a grant does not silently fail, accept or replace the user's goal.

## Durable authority and bounded limits

A grant is an explicit capability independent of its issuance login session. It
contains NO bearer token, session hash or model credential. Logging out, rotating
or revoking the original login credential does NOT revoke this delegation.
Explicitly revoke the grant, disable the principal/membership, remove required
permissions, disable the pinned profile, or wait for expiry to stop future
admission. This distinction is shown in the interface and SDK documentation.

Expiry is1–86400seconds and is clamped to the Task deadline. Status transitions
are `active`→`revoked|expired|stopped|completed`; terminal grants cannot restart.
Fresh wall-clock checks follow business-lock waits. Automatically prepared tools
and model continuations bind an exact immutable grant ID. Old queued jobs cannot
borrow a replacement grant. A provider send already admitted under its short,
nonrenewable permit may be in flight; revocation does not undo it or its bill.
The permit is at most2seconds and is clamped to grant expiry.

The worker promptly reaps known-unsent jobs bound to an inactive exact grant,
without waiting for a possibly24-hour tool expiry. It rechecks both the job and
financial Action dispatch markers under lock. Unmarked awaiting/approved/queued/
leased executions become cancelled; only their proven-unsent reservation is
released. A pending approval becomes expired; an already recorded approval is
immutable and is not rewritten. The Task returns to its exact proposal for
explicit manual review/continuation. Sent/unknown jobs, unrelated grants and
unbound manual jobs are unchanged. Lifetime allocations are never refunded.

Lifetime Task usage is cumulative across ALL grants and Runs:

- `model_turns`: automatic additional model Operations queued; limit1–31. The
  original ordinary model turn is outside this counter and still uses the
  normal Task budget and model lifetime limits32/128.
- `tool_executions`: automatic preparations; limit1–32, in addition to the
  ordinary shared tool lifetime limits32/128.
- `allocated_microusd`: the SUM of immutable upper bounds allocated to these
  preparations/continuations, limit1–1e12. It is NOT actual spend or current
  reserved balance. Settlement, refund, cancellation, Retry and new grants do
  not reduce it. New grant limits must be at least the already-used values.

Allocation and new durable work commit together. A rejected automatic step rolls
back its allocation/reservation/approval/Operation before saving a stop. Actual
model dispatch independently reserves against the Task/ancestor/organization
budgets. Pending or unknown financial obligations remain visible.

## Progress and recovery

The deterministic detector uses exact bounded semantic JSON fingerprints:
object order, whitespace, equivalent decimal spelling and call IDs cannot hide
a repeat; adjacent integers above2^53 remain different. Strings/array order and
Unicode values remain exact. Tool profile/grant refresh does not change semantic
business identity. Original approved argument bytes and approval hashes are
separate invariants and are never rewritten by fingerprinting.

Task history spans Runs and retries, with one observation per logical proposal
or manual occurrence. Soft signals include repeated requests, repeated failures,
A-B-A-B patterns and identical unverified text. Root-wide hard checks retain
unresolved unknown outcomes and prior successful modifying intents across
siblings. A new call ID, model assertion, elapsed time, receipt ID or grant does
not constitute independent progress. Duplicate intents in one batch are stopped
before automatic preparation; a proposal itself is not an observed effect.

Signals and their evidence IDs/snapshot digest are appended atomically with the
stop. Repeated coordinator ticks do not append repeated stops. History overflow,
invalid snapshots and detector errors deny automatic continuation. The current
bounded read-repeat policy can conservatively stop legitimate polling; a full
trusted wait-state/polling policy is still required. Human review and explicit
manual commands remain the recovery path. No claim of hallucination detection,
universal loop healing or universal exactly-once external effects is made.

On a failed dispatch-admission response, cleanup checks the durable marker and
fence. Proven-unsent leased work may be closed and its reserve released. A
committed marker with lost acknowledgement remains uncertain and retains its
liability; a client-side error never proves that an action was unsent.

## MCP and implementation boundaries

Read tools `platform_automation_get` and `platform_progress_signals_list` join the
default read-only catalog. `platform_automation_create` and
`platform_automation_revoke` each require explicit server opt-in and ordinary
API authorization/idempotency. The current catalog totals27reads+16mutations.
The private MCP transport profile and unfinished OAuth support are unchanged.

The Go worker performs one bounded durable coordination step per tick and no
external I/O inside a database transaction. Shared transaction helpers serve
both explicit commands and grant authority; a worker does not reconstruct a
Bearer credential or impersonate an API caller. Migration015 adds append-only
consent/signals, monotonic usage, exact job bindings and separate automatic
approval attribution. Earlier applied migrations remain unchanged.
