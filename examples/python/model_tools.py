# SPDX-License-Identifier: Apache-2.0
"""Explicit model-tool steps using an already authenticated public SDK client.

These helpers do not approve or dispatch actions. Call continuation only after
inspecting every exact tool outcome; it admits a new potentially charged model
turn. Unknown effects raise instead of being resent or fabricated as success.
"""
from agent_platform import Client, ModelToolResultBinding


def prepare_exact_proposal(client: Client, task_id: str, proposal_id: str, *, request_key: str):
    task = client.get_task(task_id)
    proposal = client.get_model_proposal(task_id, proposal_id)
    if task.get("candidate_id") != proposal["candidate_id"]:
        raise ValueError("The task has moved to another candidate; inspect it again")
    return client.prepare_tool_execution(
        task_id, occurrence_id="model:" + proposal["id"], model_proposal_id=proposal["id"],
        expected_task_version=task["version"], candidate_id=proposal["candidate_id"],
        artifact_id=proposal["artifact_id"], artifact_version=proposal["artifact_version"],
        content_digest=proposal["content_digest"], tool_id=proposal["tool_id"],
        tool_version=proposal["tool_version"], arguments_json=proposal["arguments_json"],
        expires_in_seconds=900, idempotency_key=request_key,
    )


def continue_inspected_batch(client: Client, task_id: str, *, reason: str, request_key: str):
    """The caller has explicitly chosen a new charge after inspecting results."""
    task = client.get_task(task_id)
    proposals, cursor, seen = [], None, set()
    while True:
        page = client.model_proposals(task_id, cursor=cursor)
        proposals.extend(p for p in page.data if p["candidate_id"] == task.get("candidate_id"))
        if not page.next_cursor:
            break
        cursor = page.next_cursor
        if cursor in seen or len(seen) >= 128:
            raise ValueError("Invalid or unbounded pagination")
        seen.add(cursor)
    if not proposals:
        raise ValueError("No current model-tool batch")
    results: list[ModelToolResultBinding] = []
    for proposal in proposals:
        if not proposal.get("execution_id"):
            raise ValueError("Prepare and explicitly resolve every proposal first")
        view = client.get_tool_execution(task_id, proposal["execution_id"])
        execution = view["execution"]
        result = dict(proposal_id=proposal["id"], execution_id=execution["id"],
                      execution_version=execution["version"], status=execution["status"])
        if execution["status"] in ("succeeded", "rejected"):
            result.update(receipt_id=view["receipt"]["id"], content_digest=view["receipt"]["content_digest"])
        elif execution["status"] not in ("denied", "expired", "cancelled"):
            raise ValueError("Unknown or incomplete effect blocks continuation")
        results.append(result)
    source = proposals[0]
    return client.continue_model(task_id, expected_task_version=task["version"],
                                 candidate_id=source["candidate_id"], artifact_id=source["artifact_id"],
                                 artifact_version=source["artifact_version"], content_digest=source["content_digest"],
                                 results=results, acknowledge_cost=True, reason=reason, idempotency_key=request_key)
