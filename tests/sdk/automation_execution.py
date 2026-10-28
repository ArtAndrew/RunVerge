"""SDK0.8 automation against explicitly installed synthetic local peers only."""
from __future__ import annotations
import hashlib
import json
import time
from agent_platform import APIError, Client


def exercise_automation(client: Client, key: str, wait_seconds: float) -> dict:
    model = next(m for m in client.list_models(limit=100).data if m["model"] == "native-local-fixture")
    installed = client.tools(limit=100).data
    read = next(t for t in installed if t["id"] == "tool_local_read_fixture")
    write = next(t for t in installed if t["id"] == "tool_local_action_fixture")
    assert read["active"] and read["effect"] == "read" and read.get("automation_allowed") is True and not read["require_distinct_approver"] and read["max_charge_microusd"] == "9"
    assert write["active"] and write["effect"] == "write" and write["max_charge_microusd"] == "17"
    assert 1 <= model["max_charge_microusd"] <= 10000

    def wait_task(task_id, predicate):
        end = time.monotonic() + wait_seconds
        while True:
            value = client.get_task(task_id)
            if predicate(value):return value
            if time.monotonic() >= end:raise AssertionError("Automation task did not reach expected state: " + value["status"] + ":" + value.get("waiting_reason", ""))
            time.sleep(.1)

    def cas(view):
        return dict(expected_task_version=view["task"]["version"],expected_execution_version=view["execution"]["version"],expected_approval_version=view["approval"]["version"],intent_hash=view["execution"]["intent_hash"])

    results = []
    for scenario, mode, tool in (("read_success","read_only",read),("read_unknown","read_only",read),("prepare_success","prepare_and_continue",write)):
        prefix = key + ":" + scenario
        agent = client.create_agent("Python automation " + scenario,"Use the explicitly pinned synthetic local tool, then return its observation.",model=model["model"],tools=[tool["id"]],budget_microusd=100000,idempotency_key=prefix+":agent")
        agent = client.publish_agent(agent["id"],expected_version=agent["version"],idempotency_key=prefix+":publish")
        task = client.create_task("Local automatic " + scenario,agent["id"],100000,max_runs=1,execution_timeout_seconds=600,idempotency_key=prefix+":task")
        task_id = task["id"]
        task = wait_task(task_id,lambda t:t.get("waiting_reason") == "model_tools_pending")
        run = task["current_run_id"]
        assert not client.tool_executions(task_id).data
        initial = client.automation(task_id)
        assert initial["usage"] == {"model_turns":0,"tool_executions":0,"allocated_microusd":"0"} and "grant" not in initial
        pin = agent["model_tools"][0]
        state = client.create_automation(task_id,expected_task_version=task["version"],mode=mode,tool_pins=[{k:pin[k] for k in ("tool_id","tool_version","tool_profile_digest")}],max_model_turns=4,max_tool_executions=4,max_charge_microusd=str(4*model["max_charge_microusd"]+4*int(tool["max_charge_microusd"])),expires_in_seconds=600,acknowledge_cost=True,reason="Delegate bounded synthetic local scenario",idempotency_key=prefix+":grant")
        grant_id = state["grant"]["id"]
        if mode == "prepare_and_continue":
            task = wait_task(task_id,lambda t:t.get("waiting_reason") == "tool_approval_pending")
            executions = client.tool_executions(task_id).data
            assert len(executions) == 1
            view = client.get_tool_execution(task_id,executions[0]["id"])
            assert view["execution"]["status"] == "awaiting_approval" and not view.get("receipt")
            assert client.automation(task_id)["usage"]["model_turns"] == 0
            view = client.decide_tool_execution(task_id,view["execution"]["id"],**cas(view),decision="approve",reason="Separately inspected exact synthetic write",idempotency_key=prefix+":approve")
            assert view["execution"]["status"] == "approved" and not view.get("receipt")
            client.dispatch_tool_execution(task_id,view["execution"]["id"],**cas(view),acknowledge_cost=True,idempotency_key=prefix+":dispatch")
        unknown = scenario == "read_unknown"
        final = wait_task(task_id,lambda t:t.get("waiting_reason") == ("external_action_unknown" if unknown else "model_candidate_pending"))
        executions = client.tool_executions(task_id).data
        assert len(executions) == 1
        view = client.get_tool_execution(task_id,executions[0]["id"])
        assert view["execution"]["automation_grant_id"] == grant_id
        assert view["execution"]["status"] == ("unknown" if unknown else "succeeded")
        assert view["approval"]["decision_method"] == ("task_automation_grant" if mode == "read_only" else "authorized_principal_decision")
        proposals = client.model_proposals(task_id).data
        assert len(proposals) == 1
        proposal = client.get_model_proposal(task_id,proposals[0]["id"])
        assert "9007199254740993" in proposal["arguments_json"]
        assert hashlib.sha256(proposal["arguments_json"].encode()).hexdigest() == proposal["arguments_digest"]
        assert final["current_run_id"] == run and final["attempt_count"] == 1
        assert not final.get("accepted_decision_id") and not client.decisions(task_id).data
        state = client.automation(task_id)
        end = time.monotonic()+wait_seconds
        while state["grant"]["status"] == "active" and time.monotonic()<end:
            time.sleep(.1);state=client.automation(task_id)
        assert state["grant"]["status"] != "active"
        assert state["usage"]["tool_executions"] == 1 and state["usage"]["model_turns"] == (0 if unknown else 1)
        assert state["usage"]["allocated_microusd"] == str(int(tool["max_charge_microusd"])+(0 if unknown else model["max_charge_microusd"]))
        operations_before = len(client.operations(task_id).data)
        assert operations_before == (1 if unknown else 2)
        accepted_separately = False
        denied_unknown_resend = None
        if unknown:
            try:client.dispatch_tool_execution(task_id,view["execution"]["id"],**cas(view),acknowledge_cost=True,idempotency_key=prefix+":no-resend")
            except APIError as error:assert error.status_code==409;denied_unknown_resend="409"
            else:raise AssertionError("Unknown effect was resent")
            assert any(s["code"] == "unknown_effect" for s in client.progress_signals(task_id,limit=1).data)
            time.sleep(.35)
            assert len(client.operations(task_id).data) == operations_before and len(client.tool_executions(task_id).data) == 1
            assert client.automation(task_id)["usage"] == state["usage"]
        else:
            assert view["receipt"]["method"] == "registered_tool_response"
            receipt_artifact = client.get_artifact(task_id,view["receipt"]["artifact_id"],version=1)
            if mode == "read_only":
                observed = json.loads(receipt_artifact["content"])
                assert observed["value"] == 42 and observed["read_only"] is True and observed["real_business_effect"] is False
            candidate = next(c for c in client.candidates(task_id).data if c["id"] == final["candidate_id"])
            assert candidate["source"] == "model" and candidate["run_id"] == run
            artifact = client.get_artifact(task_id,candidate["artifact_id"],version=candidate["artifact_version"])
            assert hashlib.sha256(artifact["content"].encode()).hexdigest() == candidate["content_digest"]
            assert "registered_tool_response" in artifact["content"]
            final = client.review_task(task_id,expected_version=final["version"],candidate_id=candidate["id"],artifact_id=candidate["artifact_id"],artifact_version=candidate["artifact_version"],content_digest=candidate["content_digest"],decision="accept",reason="Separately accept the synthetic fixture candidate",idempotency_key=prefix+":accept")
            assert final["status"] == "succeeded";accepted_separately = True
        cost = client.costs(task_id)
        assert cost["spent_microusd"] == "0" and cost["reserved_microusd"] == str((1 if unknown else 2)*model["max_charge_microusd"]+int(tool["max_charge_microusd"]))
        results.append({"scenario":scenario,"mode":mode,"task_id":task_id,"run_id":run,"grant_id":grant_id,"execution_id":view["execution"]["id"],"action_id":view["execution"]["action_id"],"approval_method":view["approval"]["decision_method"],"final_status":final["status"],"final_accepted_separately":accepted_separately,"unknown_resend":denied_unknown_resend,"usage":state["usage"],"model_operations":operations_before,"spent_microusd":cost["spent_microusd"],"reserved_microusd":cost["reserved_microusd"]})
    return {"sdk":"python","native_fixture_model":"native-local-fixture","read_only_tool":"tool_local_read_fixture","paid_provider_call":False,"billing_settlement":False,"results":results}
