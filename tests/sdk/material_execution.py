"""Opt-in SDK0.5 material flow against a real local API/PG/fixture worker."""
from __future__ import annotations
import hashlib
import time
from agent_platform import APIError,Client


def exercise_materials(client:Client,key:str,wait_seconds:float)->dict:
    leaf=client.create_agent("SDK material fixture","Echo only.",model="fixture:echo",tools=[],budget_microusd=10000,idempotency_key=key+":leaf")
    leaf=client.publish_agent(leaf["id"],expected_version=leaf["version"],idempotency_key=key+":leaf-publish")
    team={"pattern":"parallel","members":[{"id":"a","agent_id":leaf["id"],"agent_version":leaf["version"]},{"id":"b","agent_id":leaf["id"],"agent_version":leaf["version"]}],"join_policy":"all","max_depth":1,"max_children":2,"max_parallel":2}
    agent=client.create_agent("SDK material team","Aggregate fixture evidence.",model="team:coordinator",tools=[],team=team,budget_microusd=10000,idempotency_key=key+":team")
    agent=client.publish_agent(agent["id"],expected_version=agent["version"],idempotency_key=key+":publish-agent")
    task=client.create_task("Prepare an editable synthetic result",agent["id"],10000,max_runs=1,execution_timeout_seconds=600,idempotency_key=key+":task")
    task_id=task["id"];end=time.monotonic()+wait_seconds
    while task["status"]!="waiting_input" and time.monotonic()<end:
        assert task["status"] not in ("failed","cancelled","blocked")
        time.sleep(.15);task=client.get_task(task_id)
    assert task["status"]=="waiting_input" and task.get("candidate_id")
    original=task["candidate_id"];run=task["current_run_id"];before=client.costs(task_id)
    assert before["spent_microusd"]=="2000" and before["reserved_microusd"]=="0"
    head=client.create_material(task_id,"result.md","text/markdown","",base_candidate_id=original,idempotency_key=key+":draft")
    assert client.get_material_version(task_id,head["id"],1)["content"]==""
    assert client.get_task(task_id)["candidate_id"]==original
    first="  Synthetic summary: this is fixture evidence only.\r\n"
    head=client.revise_material(task_id,head["id"],first,expected_version=1,reason="Initial summary",idempotency_key=key+":revision-2")
    try:client.revise_material(task_id,head["id"],"stale buffer",expected_version=1,reason="Stale edit",idempotency_key=key+":stale")
    except APIError as error:assert error.status_code==409
    else:raise AssertionError("Stale material edit succeeded")
    content=first+"  Final editor conclusion; no model-quality claim.\n"
    head=client.revise_material(task_id,head["id"],content,expected_version=2,merge_base_version=1,reason="Manual merge",idempotency_key=key+":revision-3")
    head=client.get_material(task_id,head["id"])
    versions=[];cursor=None
    while True:
        page=client.material_revisions(task_id,head["id"],limit=1,cursor=cursor);versions.extend(v["version"] for v in page.data);cursor=page.next_cursor
        assert len(versions)<=128
        if cursor is None:break
    assert sorted(versions)==[1,2,3]
    assert client.get_material_version(task_id,head["id"],2)["content"]==first
    assert client.materials(task_id,limit=1).data[0]["id"]==head["id"]
    current=client.get_task(task_id);args=dict(expected_task_version=current["version"],expected_material_version=3,content_digest=hashlib.sha256(content.encode()).hexdigest(),parent_candidate_id=original,idempotency_key=key+":propose")
    proposed=client.publish_material(task_id,head["id"],**args)
    assert client.publish_material(task_id,head["id"],**args)==proposed
    assert proposed["status"]=="waiting_input" and proposed["current_run_id"]==run and proposed["attempt_count"]==1
    candidate=next(v for v in client.candidates(task_id).data if v["id"]==proposed["candidate_id"])
    assert candidate["source"]=="material" and candidate["parent_candidate_id"]==original and candidate["review_basis_candidate_id"]==original and candidate["material_version"]==3
    artifact=client.get_artifact(task_id,candidate["artifact_id"],version=candidate["artifact_version"])
    assert artifact["content"]==content and candidate["content_digest"]==hashlib.sha256(content.encode()).hexdigest()
    task=client.review_task(task_id,expected_version=proposed["version"],candidate_id=candidate["id"],artifact_id=artifact["id"],artifact_version=artifact["version"],content_digest=candidate["content_digest"],decision="accept",reason="Local fixture acceptance only",idempotency_key=key+":accept")
    assert task["status"]=="succeeded" and task["current_run_id"]==run and task["attempt_count"]==1
    head=client.revise_material(task_id,head["id"],content+"Archived annotation.\n",expected_version=3,reason="Draft remains editable after acceptance",idempotency_key=key+":revision-4")
    try:client.publish_material(task_id,head["id"],expected_task_version=task["version"],expected_material_version=4,content_digest=head["head_content_digest"],parent_candidate_id=task["candidate_id"],idempotency_key=key+":terminal-publish")
    except APIError as error:assert error.status_code==409
    else:raise AssertionError("Terminal task publication succeeded")
    after=client.costs(task_id);assert after["spent_microusd"]==before["spent_microusd"] and after["reserved_microusd"]==before["reserved_microusd"]
    assert client.get_task(task_id)["result"]==content
    return {"sdk":"python","task_id":task_id,"material_id":head["id"],"head_version":4,"published_version":3,"status":task["status"],"attempt_count":1,"run_preserved":True,"original_basis_preserved":True,"stale_edit":"409","terminal_publication":"409","publication_replay":"same cached response","spent_before_microusd":before["spent_microusd"],"synthetic_spent_microusd":after["spent_microusd"],"reserved_microusd":after["reserved_microusd"],"exact_content_digest":candidate["content_digest"],"real_provider_call":False}
