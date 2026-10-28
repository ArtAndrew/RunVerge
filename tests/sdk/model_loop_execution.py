"""Opt-in SDK0.7 flow; explicit local synthetic native model and TLS tool only."""
from __future__ import annotations
import hashlib
import time
from agent_platform import APIError, Client


def exercise_model_loop(client: Client, key: str, wait_seconds: float) -> dict:
    model = next(m for m in client.list_models(limit=100).data if m["model"] == "native-local-fixture")
    tool = next(t for t in client.tools(limit=100).data if t["id"] == "tool_local_action_fixture")
    assert tool["active"] and not tool["require_distinct_approver"] and tool["max_charge_microusd"] == "17"
    agent = client.create_agent("Python native local fixture", "Propose an installed synthetic tool, then read its exact observation.",
                                model=model["model"], tools=[tool["id"]], budget_microusd=10000, idempotency_key=key+":agent")
    assert len(agent["model_tools"]) == 1 and agent["model_tools"][0]["tool_id"] == tool["id"]
    assert not {"endpoint", "credential_environment", "trusted_ca_pem"}.intersection(agent["model_tools"][0])
    agent = client.publish_agent(agent["id"], expected_version=agent["version"], idempotency_key=key+":publish")
    results = []
    def wait_task(task_id, predicate):
        end=time.monotonic()+wait_seconds
        while True:
            value=client.get_task(task_id)
            if predicate(value): return value
            if time.monotonic()>=end: raise AssertionError("Local native fixture task did not reach expected state: "+value["status"]+":"+value.get("waiting_reason",""))
            time.sleep(.1)
    for mode in ("success", "unknown"):
        prefix=key+":"+mode
        task=client.create_task("Local native tool "+mode,agent["id"],10000,max_runs=1,execution_timeout_seconds=600,idempotency_key=prefix+":task")
        task_id=task["id"];task=wait_task(task_id,lambda t:t["waiting_reason"]=="model_tools_pending" if "waiting_reason" in t else False)
        source_run=task["current_run_id"];source_candidate=task["candidate_id"]
        assert not client.tool_executions(task_id).data,"Native model response must not automatically invoke tools"
        metadata=client.model_proposals(task_id,limit=1);assert len(metadata.data)==1 and "arguments_json" not in metadata.data[0]
        proposal=client.get_model_proposal(task_id,metadata.data[0]["id"])
        assert proposal["candidate_id"]==source_candidate and "9007199254740993" in proposal["arguments_json"]
        assert hashlib.sha256(proposal["arguments_json"].encode()).hexdigest()==proposal["arguments_digest"]
        source_op=proposal["operation_id"]
        before=client.costs(task_id);assert before["spent_microusd"]=="0" and before["reserved_microusd"]==str(model["max_charge_microusd"])
        view=client.prepare_tool_execution(task_id,occurrence_id="model:"+proposal["id"],model_proposal_id=proposal["id"],expected_task_version=task["version"],candidate_id=proposal["candidate_id"],artifact_id=proposal["artifact_id"],artifact_version=proposal["artifact_version"],content_digest=proposal["content_digest"],tool_id=proposal["tool_id"],tool_version=proposal["tool_version"],arguments_json=proposal["arguments_json"],expires_in_seconds=900,idempotency_key=prefix+":prepare")
        execution_id=view["execution"]["id"];assert view["execution"]["arguments_json"]==proposal["arguments_json"]
        def cas(v):return dict(expected_task_version=v["task"]["version"],expected_execution_version=v["execution"]["version"],expected_approval_version=v["approval"]["version"],intent_hash=v["execution"]["intent_hash"])
        view=client.decide_tool_execution(task_id,execution_id,**cas(view),decision="approve",reason="Exact native local proposal inspected",idempotency_key=prefix+":approve")
        assert view["execution"]["status"]=="approved" and not view.get("receipt")
        view=client.dispatch_tool_execution(task_id,execution_id,**cas(view),acknowledge_cost=True,idempotency_key=prefix+":dispatch")
        expected="succeeded" if mode=="success" else "unknown";end=time.monotonic()+wait_seconds
        while view["execution"]["status"]!=expected and time.monotonic()<end:
            time.sleep(.1);view=client.get_tool_execution(task_id,execution_id)
        assert view["execution"]["status"]==expected
        unknown_resend=None
        if mode=="unknown":
            try:client.dispatch_tool_execution(task_id,execution_id,**cas(view),acknowledge_cost=True,idempotency_key=prefix+":no-resend")
            except APIError as error:assert error.status_code==409;unknown_resend="409"
            else:raise AssertionError("Unknown effect was resent")
            view=client.get_tool_execution(task_id,execution_id)
            view=client.reconcile_tool_execution(task_id,execution_id,expected_task_version=view["task"]["version"],expected_execution_version=view["execution"]["version"],intent_hash=view["execution"]["intent_hash"],outcome="succeeded",effect_reference="synthetic-native-operator-observation",result="Synthetic operator assertion only",evidence_digest=hashlib.sha256(prefix.encode()).hexdigest(),reason="Record synthetic observation, not a final bill or provider verification",idempotency_key=prefix+":reconcile")
        receipt=view["receipt"];assert receipt["method"]==("registered_tool_response" if mode=="success" else "operator_assertion")
        task=client.get_task(task_id);assert task["status"]=="waiting_input" and task["candidate_id"]==source_candidate
        binding={"proposal_id":proposal["id"],"execution_id":execution_id,"execution_version":view["execution"]["version"],"status":view["execution"]["status"],"receipt_id":receipt["id"],"content_digest":receipt["content_digest"]}
        continuing=client.continue_model(task_id,expected_task_version=task["version"],candidate_id=proposal["candidate_id"],artifact_id=proposal["artifact_id"],artifact_version=proposal["artifact_version"],content_digest=proposal["content_digest"],results=[binding],acknowledge_cost=True,reason="Continue with the exact inspected synthetic observation",idempotency_key=prefix+":continue")
        assert continuing["current_run_id"]==source_run and continuing["attempt_count"]==1
        final=wait_task(task_id,lambda t:t["status"]=="waiting_input" and t.get("candidate_id") not in (None,source_candidate))
        candidate=next(c for c in client.candidates(task_id).data if c["id"]==final["candidate_id"])
        assert candidate["source"]=="model" and candidate["operation_id"]!=source_op and candidate["run_id"]==source_run
        artifact=client.get_artifact(task_id,candidate["artifact_id"],version=candidate["artifact_version"])
        assert receipt["method"] in artifact["content"] and hashlib.sha256(artifact["content"].encode()).hexdigest()==candidate["content_digest"]
        assert not final.get("accepted_decision_id") and not client.decisions(task_id).data
        accepted=client.review_task(task_id,expected_version=final["version"],candidate_id=candidate["id"],artifact_id=candidate["artifact_id"],artifact_version=candidate["artifact_version"],content_digest=candidate["content_digest"],decision="accept",reason="Separately accept synthetic fixture prose",idempotency_key=prefix+":accept")
        cost=client.costs(task_id);assert accepted["status"]=="succeeded" and cost["spent_microusd"]=="0" and cost["reserved_microusd"]==str(2*model["max_charge_microusd"]+17)
        results.append({"mode":mode,"task_id":task_id,"proposal_id":proposal["id"],"execution_id":execution_id,"initial_operation_id":source_op,"continued_operation_id":candidate["operation_id"],"run_id":source_run,"receipt_method":receipt["method"],"unknown_resend":unknown_resend,"arguments_digest":proposal["arguments_digest"],"final_candidate_id":candidate["id"],"final_accepted_separately":True,"status":accepted["status"],"spent_microusd":cost["spent_microusd"],"reserved_microusd":cost["reserved_microusd"]})
    return {"sdk":"python","native_fixture_model":"native-local-fixture","paid_provider_call":False,"billing_settlement":False,"results":results}
