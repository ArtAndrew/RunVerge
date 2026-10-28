"""Opt-in SDK0.6 scenario using an explicitly installed local synthetic tool."""
from __future__ import annotations
import hashlib
import time
from agent_platform import APIError, Client


def exercise_tools(client: Client, key: str, wait_seconds: float, fixtures: dict) -> dict:
    catalog = []; cursor = None
    while True:
        page = client.tools(cursor=cursor, limit=1); catalog.extend(page.data); cursor = page.next_cursor
        assert len(catalog) <= 128
        if cursor is None: break
    for fixture in fixtures.values():
        tool = next(item for item in catalog if item["id"] == fixture["tool_id"])
        assert tool["active"] and not tool["require_distinct_approver"]
        assert tool["max_charge_microusd"] == "17", "Only the explicitly installed 17-microUSD local fixture is allowed"
        assert not {"endpoint", "credential_environment", "private_ca_pem"}.intersection(tool)
    leaf = client.create_agent("SDK tool fixture", "Echo synthetic evidence only.", model="fixture:echo", tools=[], budget_microusd=10000, idempotency_key=key+":leaf")
    leaf = client.publish_agent(leaf["id"], expected_version=leaf["version"], idempotency_key=key+":leaf-publish")
    team = {"pattern":"parallel", "members":[{"id":"first","agent_id":leaf["id"],"agent_version":leaf["version"]},{"id":"second","agent_id":leaf["id"],"agent_version":leaf["version"]}],"join_policy":"all","max_depth":1,"max_children":2,"max_parallel":2}
    agent = client.create_agent("SDK exact tool team", "Aggregate synthetic evidence.", model="team:coordinator", tools=[], team=team, budget_microusd=10000, idempotency_key=key+":team")
    agent = client.publish_agent(agent["id"], expected_version=agent["version"], idempotency_key=key+":publish")
    results=[]
    for mode in ("success", "unknown"):
        fixture=fixtures[mode]; tool=next(item for item in catalog if item["id"]==fixture["tool_id"]); prefix=key+":"+mode
        task=client.create_task("Check synthetic tool "+mode,agent["id"],10000,max_runs=1,execution_timeout_seconds=600,idempotency_key=prefix+":task")
        task_id=task["id"];end=time.monotonic()+wait_seconds
        while task["status"]!="waiting_input" and time.monotonic()<end:
            assert task["status"] not in ("blocked","failed","cancelled")
            time.sleep(.1);task=client.get_task(task_id)
        assert task["status"]=="waiting_input"
        candidate=next(item for item in client.candidates(task_id).data if item["id"]==task["candidate_id"])
        artifact=client.get_artifact(task_id,candidate["artifact_id"],version=candidate["artifact_version"])
        assert hashlib.sha256(artifact["content"].encode()).hexdigest()==candidate["content_digest"]
        before=client.costs(task_id);assert before["spent_microusd"]=="2000" and before["reserved_microusd"]=="0"
        original_run=task["current_run_id"];original_candidate=task["candidate_id"]
        view=client.prepare_tool_execution(task_id,occurrence_id=prefix,expected_task_version=task["version"],candidate_id=candidate["id"],artifact_id=artifact["id"],artifact_version=artifact["version"],content_digest=candidate["content_digest"],tool_id=tool["id"],tool_version=tool["version"],arguments_json=fixture["arguments_json"],expires_in_seconds=900,idempotency_key=prefix+":prepare")
        execution_id=view["execution"]["id"];assert view["task"]["version"]>task["version"] and view["task"]["status"]=="waiting_approval"
        assert view["execution"]["arguments_json"]==fixture["arguments_json"]
        assert "arguments_json" not in client.tool_executions(task_id,limit=1).data[0]
        view=client.get_tool_execution(task_id,execution_id); intent=view["execution"]["intent_hash"]
        def cas(v): return dict(expected_task_version=v["task"]["version"],expected_execution_version=v["execution"]["version"],expected_approval_version=v["approval"]["version"],intent_hash=v["execution"]["intent_hash"])
        view=client.decide_tool_execution(task_id,execution_id,**cas(view),decision="approve",reason="Exact local fixture intent reviewed. "+"x"*300,idempotency_key=prefix+":approve")
        assert view["execution"]["status"]=="approved" and view["task"]["status"]=="waiting_approval" and not view.get("receipt")
        view=client.dispatch_tool_execution(task_id,execution_id,**cas(view),acknowledge_cost=True,idempotency_key=prefix+":dispatch")
        end=time.monotonic()+wait_seconds
        expected="succeeded" if mode=="success" else "unknown"
        while view["execution"]["status"]!=expected and time.monotonic()<end:
            assert view["execution"]["status"] in ("queued","leased","sent",expected)
            time.sleep(.1);view=client.get_tool_execution(task_id,execution_id)
        assert view["execution"]["status"]==expected and view["execution"]["intent_hash"]==intent
        unknown_replay=None
        if mode=="unknown":
            assert view["task"]["status"]=="blocked" and view["task"]["waiting_reason"]=="external_action_unknown"
            try:client.dispatch_tool_execution(task_id,execution_id,**cas(view),acknowledge_cost=True,idempotency_key=prefix+":forbidden-resend")
            except APIError as error:assert error.status_code==409;unknown_replay="409"
            else:raise AssertionError("Unknown tool execution was queued again")
            view=client.get_tool_execution(task_id,execution_id)
            view=client.reconcile_tool_execution(task_id,execution_id,expected_task_version=view["task"]["version"],expected_execution_version=view["execution"]["version"],intent_hash=intent,outcome="rejected",effect_reference="local-fixture-operator-observation",result="",evidence_digest=hashlib.sha256((prefix+":assertion").encode()).hexdigest(),reason="Synthetic operator assertion, not a verified external fact or bill.",idempotency_key=prefix+":reconcile")
            assert view["receipt"]["method"]=="operator_assertion"
        else:assert view["receipt"]["method"]=="registered_tool_response"
        exact=client.get_artifact(task_id,view["receipt"]["artifact_id"],version=1)
        assert hashlib.sha256(exact["content"].encode()).hexdigest()==view["receipt"]["content_digest"]
        after=client.costs(task_id);fresh=client.get_task(task_id)
        assert after["spent_microusd"]==before["spent_microusd"] and after["reserved_microusd"]=="17"
        assert fresh["status"]=="waiting_input" and fresh["current_run_id"]==original_run and fresh["candidate_id"]==original_candidate and not fresh.get("accepted_decision_id")
        assert not client.decisions(task_id).data
        results.append({"mode":mode,"task_id":task_id,"execution_id":execution_id,"execution_status":view["execution"]["status"],"task_status":fresh["status"],"receipt_method":view["receipt"]["method"],"source_run_preserved":True,"candidate_not_accepted":True,"spent_microusd":after["spent_microusd"],"reserved_microusd":after["reserved_microusd"],"unknown_resend":unknown_replay,"arguments_digest":view["execution"]["arguments_digest"]})
    return {"sdk":"python","fixture_tool_only":True,"paid_model_call":False,"billing_settlement":False,"results":results}
