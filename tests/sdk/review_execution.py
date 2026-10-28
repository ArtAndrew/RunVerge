"""Opt-in 0.5 SDK flow through a real platform and synthetic fixture team."""
from __future__ import annotations

import hashlib
import time
from agent_platform import APIError, Client


def exercise_review(client: Client, request_key: str, wait_seconds: float) -> dict:
    leaf = client.create_agent("SDK review fixture", "Repeat the execution input exactly.", model="fixture:echo", tools=[], budget_microusd=10000, idempotency_key=request_key+":leaf")
    leaf = client.publish_agent(leaf["id"], expected_version=leaf["version"], idempotency_key=request_key+":leaf-publish")
    team = {"pattern":"parallel","members":[{"id":"first","agent_id":leaf["id"],"agent_version":leaf["version"],"task":"Prepare first evidence"},{"id":"second","agent_id":leaf["id"],"agent_version":leaf["version"],"task":"Prepare second evidence"}],"join_policy":"all","max_depth":1,"max_children":2,"max_parallel":2}
    agent = client.create_agent("SDK review team", "Aggregate synthetic fixture materials.", model="team:coordinator", tools=[], team=team, budget_microusd=10000, idempotency_key=request_key+":team")
    agent = client.publish_agent(agent["id"], expected_version=agent["version"], idempotency_key=request_key+":publish")
    task = client.create_task("Exercise explicit candidate review and a new attempt using only fixtures", agent["id"], 10000, max_runs=2, execution_timeout_seconds=600, idempotency_key=request_key+":task")
    task_id=task["id"]
    def wait_candidate():
        deadline=time.monotonic()+wait_seconds
        while time.monotonic()<deadline:
            current=client.get_task(task_id)
            if current["status"]=="waiting_input" and current.get("candidate_id"): return current
            if current["status"] in ("failed","cancelled","blocked"): raise AssertionError("Fixture team stopped before a candidate: "+current["status"]+"/"+current.get("waiting_reason",""))
            time.sleep(.15)
        raise AssertionError("Timed out awaiting fixture-team candidate")
    def binding(current):
        candidate=next(v for v in client.candidates(task_id).data if v["id"]==current["candidate_id"])
        artifact=client.get_artifact(task_id,candidate["artifact_id"],version=candidate["artifact_version"])
        assert candidate["source"]=="team" and candidate["run_id"]==current["current_run_id"]
        assert hashlib.sha256(artifact["content"].encode()).hexdigest()==candidate["content_digest"]
        return dict(expected_version=current["version"],candidate_id=candidate["id"],artifact_id=artifact["id"],artifact_version=artifact["version"],content_digest=candidate["content_digest"])
    task=wait_candidate();first_run=task["current_run_id"];initial=client.costs(task_id)
    assert task["attempt_count"]==1 and task["max_runs"]==2 and initial["spent_microusd"]=="2000"
    blocked=client.review_task(task_id,**binding(task),decision="request_changes",reason="Add an explicit limitations statement",idempotency_key=request_key+":review")
    assert blocked["status"]=="blocked" and blocked["current_run_id"]==first_run and blocked["attempt_count"]==1
    assert client.costs(task_id)["spent_microusd"]=="2000", "Request changes must not launch another execution"
    feedback="Label all evidence as a synthetic fixture and add its limitations."
    retry_args=dict(expected_version=blocked["version"],reason="Check a new bounded attempt",feedback=feedback,acknowledge_new_cost=True,idempotency_key=request_key+":retry")
    queued=client.retry_task(task_id,**retry_args)
    assert client.retry_task(task_id,**retry_args)==queued, "Replay must return the cached attempt, not create another"
    task=wait_candidate();assert task["attempt_count"]==2 and task["current_run_id"]!=first_run
    run=next(v for v in client.runs(task_id).data if v["id"]==task["current_run_id"])
    assert run["feedback"]==feedback and run["source_candidate_id"]==blocked["candidate_id"] and len(run["input_digest"])==64
    try: client.review_task(task_id,**binding(blocked),decision="accept",idempotency_key=request_key+":stale-review")
    except APIError as error: assert error.status_code==409
    else: raise AssertionError("Stale candidate accepted")
    task=client.review_task(task_id,**binding(task),decision="accept",reason="Accept fixture output for the local test only",idempotency_key=request_key+":accept")
    assert task["status"]=="succeeded" and task["accepted_decision_id"]
    decisions=client.decisions(task_id).data;assert len(decisions)==2
    assert {v["decision"] for v in decisions}=={"request_changes","accept"}
    assert all(v["method"]=="authorized_user_decision" for v in decisions)
    children=[];cursor=None
    while True:
        page=client.children(task_id,cursor=cursor,limit=1);children+=page.data;cursor=page.next_cursor
        assert len(children)<=128
        if cursor is None: break
    assert len(children)==4 and len({v["id"] for v in children})==4
    costs=client.costs(task_id);assert costs["spent_microusd"]=="4000" and costs["reserved_microusd"]=="0"
    assert all(v["synthetic"] is True for v in costs["entries"])
    return {"sdk":"python","task_id":task_id,"status":task["status"],"attempt_count":task["attempt_count"],"children":len(children),"candidates":len(client.candidates(task_id).data),"decisions":len(decisions),"initial_synthetic_spent_microusd":initial["spent_microusd"],"synthetic_spent_microusd":costs["spent_microusd"],"reserved_microusd":costs["reserved_microusd"],"retry_replay":"same cached response","stale_candidate":"409","feedback_pinned":True,"real_provider_call":False}
