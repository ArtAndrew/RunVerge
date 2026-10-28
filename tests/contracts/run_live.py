# SPDX-License-Identifier: Elastic-2.0
"""Validate actual current HTTP responses; fixture provider only, never real inference."""
from __future__ import annotations

import argparse
import hashlib
import ipaddress
import json
from pathlib import Path
import re
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

from support import ROOT, read_json, response_validator


class NoRedirects(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, file, code, message, headers, url):
        raise RuntimeError("contract probe refused redirect")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default="http://127.0.0.1:8080")
    parser.add_argument("--credentials", type=Path, default=ROOT / ".local/bootstrap.json")
    parser.add_argument("--report", type=Path, default=ROOT / ".local/qa/contract-live-report.json")
    parser.add_argument("--tool-fixtures", type=Path, default=ROOT / "tests/fixtures/tool-scenarios.json")
    parser.add_argument("--preflight-only", action="store_true")
    args = parser.parse_args()
    url = urllib.parse.urlsplit(args.base_url)
    try:
        loopback = ipaddress.ip_address(url.hostname or "").is_loopback
    except ValueError:
        loopback = False
    if url.scheme != "http" or not loopback or url.username or url.password or url.path or url.query or url.fragment:
        raise SystemExit("This fixture probe accepts a literal loopback HTTP origin only")
    openapi = read_json(ROOT / "contracts/openapi.json")
    version = openapi["info"]["version"]
    if not isinstance(version, str) or not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", version):
        raise SystemExit("Generate valid public contract version metadata before running the live probe")
    client = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirects())
    observations = []

    def request(method, path, route=None, body=None, expected=200, authenticated=True, raw=None, key=None):
        data = raw if raw is not None else (json.dumps(body, ensure_ascii=False).encode() if body is not None else None)
        headers = {"Accept": "application/json", "X-Platform-API-Version": version}
        if authenticated:
            headers.update({"Authorization": f"Bearer {token}", "X-Organization-ID": org})
        if data is not None:
            headers["Content-Type"] = "application/json"
            headers["Idempotency-Key"] = key or str(uuid.uuid4())
        req = urllib.request.Request(args.base_url + path, data=data, headers=headers, method=method)
        try:
            response = client.open(req, timeout=5)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            status = response.status
            if path.startswith("/api/v1/") and response.headers.get_all("X-Platform-API-Version", []) != [version]:
                raise RuntimeError("Incompatible or ambiguous API response version")
            payload = response.read(2 * 1024 * 1024 + 1)
            if len(payload) > 2 * 1024 * 1024:
                raise AssertionError("contract response exceeds probe bound")
            document = json.loads(payload)
        if status != expected:
            raise AssertionError(f"{method} {route or path}: expected HTTP {expected}, received {status}")
        if route:
            errors = list(response_validator(openapi, route, method.lower(), status).iter_errors(document))
            if errors:
                # Report locations and keywords only, never raw response values.
                locations = ["/".join(str(p) for p in e.absolute_path) + ":" + str(e.validator) for e in errors]
                raise AssertionError(f"{method} {route}: schema mismatch at {locations}")
            data_value = document.get("data")
            observations.append({"method": method, "route": route, "status": status, "items": len(data_value) if isinstance(data_value, list) else None})
        return document

    health = request("GET", "/healthz", authenticated=False)
    if health.get("data", {}).get("contract_version") != version:
        raise SystemExit(f"Require API {version}; no live fixture was created")
    if args.preflight_only:
        print(json.dumps({"contract_version": version, "mutations": 0,
                          "scope": "health version only; credentials, peers and permissions not checked"}))
        return
    credentials = read_json(args.credentials)
    token = credentials["token"]
    org = credentials["organization_id"]
    tool_fixtures = read_json(args.tool_fixtures)
    assert set(tool_fixtures) == {"success", "unknown"}
    assert all(set(value) == {"tool_id", "arguments_json"} and value["tool_id"] == "tool_local_action_fixture" and isinstance(value["arguments_json"], str) for value in tool_fixtures.values()), "Require explicitly installed local synthetic tool manifest"

    request("GET", "/api/v1/tasks", "/tasks", authenticated=False, expected=401)
    identity = request("GET", "/api/v1/me", "/me")["data"]
    assert identity["organization_id"] == credentials["organization_id"] and identity["principal_id"]
    assert not ({"token", "csrf_token", "credential"} & identity.keys())
    # Organization management provisions a new principal, never attaches an
    # arbitrary existing global identity. These calls perform no inference.
    capabilities=request("GET","/api/v1/members/capabilities","/members/capabilities")["data"]
    assert capabilities["can_create_members"] and not capabilities["self_management_allowed"]
    member_key=str(uuid.uuid4()); member_body={"display_name":"Schema membership fixture","role":"member","reason":"Controlled local contract provisioning"}
    member=request("POST","/api/v1/members","/members",body=member_body,key=member_key,expected=201)["data"]
    assert request("POST","/api/v1/members","/members",body=member_body,key=member_key,expected=201)["data"]==member
    member_path="/api/v1/members/"+member["principal_id"]
    assert request("GET",member_path,"/members/{principal_id}")["data"]["principal_id"]==member["principal_id"]
    request("GET","/api/v1/members?limit=1","/members")
    update={"expected_version":member["version"],"display_name":member["display_name"],"role":"viewer","active":False,"reason":"Exact false and role through wire"}
    request("POST",member_path+"/update","/members/{principal_id}/update",body={k:v for k,v in update.items() if k!="active"},expected=400)
    member=request("POST",member_path+"/update","/members/{principal_id}/update",body=update)["data"]
    assert member["active"] is False and member["role"]=="viewer"
    member=request("POST",member_path+"/revoke-credentials","/members/{principal_id}/revoke-credentials",body={"expected_version":member["version"],"reason":"Credential-only wire contract check"})["data"]
    audit=request("GET","/api/v1/membership-audit?principal_id="+member["principal_id"],"/membership-audit")["data"]
    assert len(audit)==3 and all(v["target_principal_id"]==member["principal_id"] for v in audit)
    membership_flow={"principal_id":member["principal_id"],"final_version":member["version"],"audit_rows":len(audit),"active_false_preserved":True,"no_login_secret_returned":not {"token","session","credential_generation","security_epoch"}.intersection(member)}
    models = request("GET", "/api/v1/models?limit=100", "/models")["data"]
    request("GET", "/api/v1/tasks/nonexistent-contract-fixture", "/tasks/{id}", expected=404)
    name = "Schema fixture " + uuid.uuid4().hex[:8]
    agent = request("POST", "/api/v1/agents", "/agents", body={"name": name, "instructions": "Return a labeled synthetic fixture result.", "model": "fixture:echo", "tools": [], "budget_microusd": 10000}, expected=201)["data"]
    published = request("POST", f"/api/v1/agents/{agent['id']}/publish", "/agents/{id}/publish", body={"expected_version": agent["version"]})["data"]
    request("GET", "/api/v1/agents?limit=1", "/agents")
    task_body = {"goal": "Validate actual API response schemas using fixture:echo only", "agent_id": published["id"], "budget_microusd": 10000}
    task_key = str(uuid.uuid4())
    task = request("POST", "/api/v1/tasks", "/tasks", body=task_body, key=task_key, expected=201)["data"]
    request("POST", "/api/v1/tasks", "/tasks", body={**task_body, "goal": "changed fixture intent"}, key=task_key, expected=409)
    task_id = task["id"]
    deadline = time.monotonic() + 15
    while task["status"] not in {"succeeded", "failed", "cancelled"} and time.monotonic() < deadline:
        time.sleep(0.15)
        task = request("GET", f"/api/v1/tasks/{task_id}", "/tasks/{id}")["data"]
    if task["status"] != "succeeded":
        raise AssertionError("Fixture worker did not complete the probe task; success-path checks incomplete")
    request("GET", "/api/v1/tasks?limit=1", "/tasks")
    object_counts = {"models": len(models)}
    for suffix in ("events", "artifacts", "runs", "operations", "actions", "approvals", "candidates", "decisions", "children"):
        query = "?after=0&limit=100" if suffix == "events" else ""
        result = request("GET", f"/api/v1/tasks/{task_id}/{suffix}{query}", f"/tasks/{{id}}/{suffix}")
        object_counts[suffix] = len(result["data"])
    if not all(object_counts[x] > 0 for x in ("events", "artifacts", "runs", "operations")):
        raise AssertionError("Expected nonempty durable fixture responses are missing")
    costs = request("GET", f"/api/v1/tasks/{task_id}/costs", "/tasks/{id}/costs")["data"]
    object_counts.update({name: len(costs[name]) for name in ("reservations", "entries", "incidents", "quarantines")})
    if not costs["reservations"] or not costs["entries"]:
        raise AssertionError("Fixture cost responses are empty; financial schemas unexercised")
    if costs["incomplete"] or costs["pending_quarantine_count"] != 0:
        raise AssertionError("Synthetic fixture unexpectedly has pending financial reconciliation")
    if task["spent_microusd"] != "1000" or costs["spent_microusd"] != "1000":
        raise AssertionError("Fixture cost must be an exact decimal-string financial fact")
    # Completed cancellation is an idempotent terminal read, valid even if work won the race.
    request("POST", f"/api/v1/tasks/{task_id}/cancel", "/tasks/{id}/cancel", body={})

    # A real coordinator/fixture-worker path provides a reviewable team draft
    # without a paid model or an untrusted accounting submission.
    team_definition = {"pattern": "parallel", "members": [
        {"id": "first", "agent_id": published["id"], "agent_version": published["version"], "task": "Prepare first evidence"},
        {"id": "second", "agent_id": published["id"], "agent_version": published["version"], "task": "Prepare second evidence"},
    ], "join_policy": "all", "max_depth": 1, "max_children": 2, "max_parallel": 2}
    team = request("POST", "/api/v1/agents", "/agents", body={"name": name+" team", "instructions": "Aggregate synthetic fixture evidence", "model": "team:coordinator", "tools": [], "budget_microusd": 10000, "team": team_definition}, expected=201)["data"]
    team = request("POST", f"/api/v1/agents/{team['id']}/publish", "/agents/{id}/publish", body={"expected_version": team["version"]})["data"]
    review_task = request("POST", "/api/v1/tasks", "/tasks", body={"goal": "Review a synthetic team candidate", "agent_id": team["id"], "budget_microusd": 10000, "max_runs": 2, "execution_timeout_seconds": 600}, expected=201)["data"]
    review_id = review_task["id"]
    def current_candidate():
        end = time.monotonic() + 30
        while time.monotonic() < end:
            current = request("GET", f"/api/v1/tasks/{review_id}", "/tasks/{id}")["data"]
            if current["status"] == "waiting_input" and current.get("candidate_id"):
                candidates = request("GET", f"/api/v1/tasks/{review_id}/candidates", "/tasks/{id}/candidates")["data"]
                candidate = next(v for v in candidates if v["id"] == current["candidate_id"])
                artifacts = request("GET", f"/api/v1/tasks/{review_id}/artifacts", "/tasks/{id}/artifacts")["data"]
                artifact = request("GET", f"/api/v1/tasks/{review_id}/artifacts/{candidate['artifact_id']}?version={candidate['artifact_version']}", "/tasks/{id}/artifacts/{artifact_id}")["data"]
                assert candidate["source"] == "team" and candidate["run_id"] == current["current_run_id"]
                assert hashlib.sha256(artifact["content"].encode()).hexdigest() == candidate["content_digest"]
                return current, {"expected_version": current["version"], "candidate_id": candidate["id"], "artifact_id": artifact["id"], "artifact_version": artifact["version"], "content_digest": candidate["content_digest"]}
            assert current["status"] not in ("failed", "cancelled", "blocked"), "Fixture team stopped before its candidate"
            time.sleep(.15)
        raise AssertionError("Fixture team did not produce its candidate")
    review_task, first_binding = current_candidate()
    assert review_task["attempt_count"] == 1
    before_costs = request("GET", f"/api/v1/tasks/{review_id}/costs", "/tasks/{id}/costs")["data"]
    assert before_costs["spent_microusd"] == "2000"
    blocked = request("POST", f"/api/v1/tasks/{review_id}/review", "/tasks/{id}/review", body={**first_binding, "decision": "request_changes", "reason": "Add explicit fixture limitations"})["data"]
    assert blocked["status"] == "blocked" and blocked["attempt_count"] == 1
    request("POST", f"/api/v1/tasks/{review_id}/retry", "/tasks/{id}/retry", body={"expected_version": blocked["version"], "reason": "Revise", "feedback": "Label evidence as synthetic", "acknowledge_new_cost": False}, expected=400)
    feedback = "Label evidence as synthetic and explain the fixture limitations."
    retry_body = {"expected_version": blocked["version"], "reason": "Revise explicitly", "feedback": feedback, "acknowledge_new_cost": True}
    retry_key = str(uuid.uuid4())
    queued = request("POST", f"/api/v1/tasks/{review_id}/retry", "/tasks/{id}/retry", body=retry_body, key=retry_key)["data"]
    assert request("POST", f"/api/v1/tasks/{review_id}/retry", "/tasks/{id}/retry", body=retry_body, key=retry_key)["data"] == queued
    review_task, second_binding = current_candidate()
    assert review_task["attempt_count"] == 2 and second_binding["candidate_id"] != first_binding["candidate_id"]
    request("POST", f"/api/v1/tasks/{review_id}/review", "/tasks/{id}/review", body={**first_binding, "decision": "accept"}, expected=409)
    # Draft edits and publication reuse the existing reviewable team run.
    material_collection = f"/api/v1/tasks/{review_id}/materials"
    for invalid in ({"name": "Missing content", "media_type": "text/plain"}, {"name": "Null content", "media_type": "text/plain", "content": None}):
        request("POST", material_collection, "/tasks/{id}/materials", body=invalid, expected=400)
    material = request("POST", material_collection, "/tasks/{id}/materials", body={"name": "contract-result.md", "media_type": "text/markdown", "content": "", "base_candidate_id": second_binding["candidate_id"]}, expected=201)["data"]
    material_path = material_collection + "/" + material["id"]
    material_route = "/tasks/{id}/materials/{material_id}"
    request("GET", material_collection+"?limit=1", "/tasks/{id}/materials")
    request("GET", material_path, material_route)
    first_version = request("GET", material_path+"/revisions/1", material_route+"/revisions/{version}")["data"]
    assert first_version["content"] == "" and first_version["revision"]["version"] == 1
    request("POST", material_path+"/publish", material_route+"/publish", body={"expected_task_version": review_task["version"], "expected_material_version": 1, "content_digest": material["head_content_digest"], "parent_candidate_id": second_binding["candidate_id"]}, expected=400)
    first_text = "  Synthetic fixture summary.\r\n"
    material = request("POST", material_path+"/revisions", material_route+"/revisions", body={"expected_version": 1, "content": first_text, "reason": "Initial draft"}, expected=201)["data"]
    request("POST", material_path+"/revisions", material_route+"/revisions", body={"expected_version": 1, "content": "stale local buffer", "reason": "Stale edit"}, expected=409)
    final_text = first_text + "Editor conclusion remains unverified fixture text.\n"
    material = request("POST", material_path+"/revisions", material_route+"/revisions", body={"expected_version": 2, "merge_base_version": 1, "content": final_text, "reason": "Explicit manual merge"}, expected=201)["data"]
    assert request("GET", material_path+"/revisions/2", material_route+"/revisions/{version}")["data"]["content"] == first_text
    revision_versions, revision_cursor = [], None
    while True:
        query = "?limit=1" + ("&cursor="+urllib.parse.quote(revision_cursor, safe="") if revision_cursor else "")
        page = request("GET", material_path+"/revisions"+query, material_route+"/revisions")
        revision_versions += [item["version"] for item in page["data"]]
        revision_cursor = page.get("next_cursor")
        assert len(revision_versions) <= 128
        if revision_cursor is None:
            break
    assert sorted(revision_versions) == [1, 2, 3]
    parent_candidate = second_binding["candidate_id"]
    review_task = request("GET", f"/api/v1/tasks/{review_id}", "/tasks/{id}")["data"]
    publication_body = {"expected_task_version": review_task["version"], "expected_material_version": 3, "content_digest": material["head_content_digest"], "parent_candidate_id": parent_candidate}
    publication_key = str(uuid.uuid4())
    proposed = request("POST", material_path+"/publish", material_route+"/publish", body=publication_body, key=publication_key)["data"]
    assert request("POST", material_path+"/publish", material_route+"/publish", body=publication_body, key=publication_key)["data"] == proposed
    assert proposed["current_run_id"] == review_task["current_run_id"] and proposed["attempt_count"] == 2 and proposed["status"] == "waiting_input"
    candidate = next(value for value in request("GET", f"/api/v1/tasks/{review_id}/candidates", "/tasks/{id}/candidates")["data"] if value["id"] == proposed["candidate_id"])
    assert candidate["source"] == "material" and candidate["review_basis_candidate_id"] == parent_candidate and candidate["parent_candidate_id"] == parent_candidate and candidate["material_version"] == 3
    exact = request("GET", f"/api/v1/tasks/{review_id}/artifacts/{candidate['artifact_id']}?version={candidate['artifact_version']}", "/tasks/{id}/artifacts/{artifact_id}")["data"]
    assert exact["content"] == final_text and hashlib.sha256(final_text.encode()).hexdigest() == candidate["content_digest"]
    second_binding = {"expected_version": proposed["version"], "candidate_id": candidate["id"], "artifact_id": exact["id"], "artifact_version": exact["version"], "content_digest": candidate["content_digest"]}
    material_flow = {"material_id": material["id"], "revision_count": 3, "published_version": 3, "empty_draft_allowed": True, "empty_publication": "400", "stale_edit": "409", "merge_base_version": 1, "publication_replay": "same cached response", "original_basis_preserved": True, "run_preserved": True}
    accepted = request("POST", f"/api/v1/tasks/{review_id}/review", "/tasks/{id}/review", body={**second_binding, "decision": "accept", "reason": "Accept the synthetic fixture result for this check"})["data"]
    assert accepted["status"] == "succeeded" and accepted["accepted_decision_id"]
    decisions = request("GET", f"/api/v1/tasks/{review_id}/decisions", "/tasks/{id}/decisions")["data"]
    assert len(decisions) == 2 and {v["decision"] for v in decisions} == {"request_changes", "accept"}
    assert all(v["method"] == "authorized_user_decision" for v in decisions)
    children = request("GET", f"/api/v1/tasks/{review_id}/children", "/tasks/{id}/children")["data"]
    assert len(children) == 4
    after_costs = request("GET", f"/api/v1/tasks/{review_id}/costs", "/tasks/{id}/costs")["data"]
    assert after_costs["spent_microusd"] == "4000" and after_costs["reserved_microusd"] == "0"
    runs = request("GET", f"/api/v1/tasks/{review_id}/runs", "/tasks/{id}/runs")["data"]
    newest = next(v for v in runs if v["id"] == accepted["current_run_id"])
    assert newest["feedback"] == feedback and newest["source_candidate_id"] == first_binding["candidate_id"]
    collaboration = {"task_id": review_id, "status": accepted["status"], "attempt_count": accepted["attempt_count"], "children": len(children), "decisions": len(decisions), "initial_synthetic_spent_microusd": before_costs["spent_microusd"], "synthetic_spent_microusd": after_costs["spent_microusd"], "retry_replay": "same cached response", "stale_candidate": "409", "feedback_pinned": True}

    catalog, tool_cursor = [], None
    while True:
        query = "?limit=1" + ("&cursor="+urllib.parse.quote(tool_cursor, safe="") if tool_cursor else "")
        page = request("GET", "/api/v1/tools"+query, "/tools")
        catalog.extend(page["data"]); tool_cursor = page.get("next_cursor")
        assert len(catalog) <= 128
        if not tool_cursor: break
    tool_flows = []
    for mode in ("success", "unknown"):
        fixture=tool_fixtures[mode];tool=next(item for item in catalog if item["id"]==fixture["tool_id"])
        assert tool["active"] and not tool["require_distinct_approver"] and tool["max_charge_microusd"]=="17"
        assert not {"endpoint", "credential_environment", "private_ca_pem"}.intersection(tool)
        source=request("POST", "/api/v1/tasks", "/tasks", body={"goal":"Contract local tool "+mode,"agent_id":team["id"],"budget_microusd":10000,"max_runs":1,"execution_timeout_seconds":600}, expected=201)["data"]
        source_id=source["id"];source_path=f"/api/v1/tasks/{source_id}";end=time.monotonic()+60
        while source["status"]!="waiting_input" and time.monotonic()<end:
            assert source["status"] not in ("blocked","failed","cancelled")
            time.sleep(.1);source=request("GET",source_path,"/tasks/{id}")["data"]
        assert source["status"]=="waiting_input"
        source_candidate=next(item for item in request("GET",source_path+"/candidates","/tasks/{id}/candidates")["data"] if item["id"]==source["candidate_id"])
        collection=source_path+"/tool-executions";collection_route="/tasks/{id}/tool-executions"
        prepare={"occurrence_id":"contract-tool-"+uuid.uuid4().hex,"expected_task_version":source["version"],"candidate_id":source_candidate["id"],"artifact_id":source_candidate["artifact_id"],"artifact_version":source_candidate["artifact_version"],"content_digest":source_candidate["content_digest"],"tool_id":tool["id"],"tool_version":tool["version"],"arguments_json":fixture["arguments_json"],"expires_in_seconds":900}
        request("POST",collection,collection_route,body={**prepare,"arguments_json":"[]"},expected=400)
        view=request("POST",collection,collection_route,body=prepare,expected=201)["data"]
        execution_path=collection+"/"+view["execution"]["id"];execution_route=collection_route+"/{execution_id}"
        metadata=request("GET",collection+"?limit=1",collection_route)["data"];assert len(metadata)==1 and "arguments_json" not in metadata[0]
        view=request("GET",execution_path,execution_route)["data"]
        assert view["execution"]["arguments_json"]==fixture["arguments_json"] and view["execution"]["arguments_digest"]==hashlib.sha256(fixture["arguments_json"].encode()).hexdigest()
        def tool_cas(v):return {"expected_task_version":v["task"]["version"],"expected_execution_version":v["execution"]["version"],"expected_approval_version":v["approval"]["version"],"intent_hash":v["execution"]["intent_hash"]}
        view=request("POST",execution_path+"/decide",execution_route+"/decide",body={**tool_cas(view),"decision":"approve","reason":"Exact local fixture reviewed. "+"x"*300})["data"]
        assert view["execution"]["status"]=="approved" and not view.get("receipt")
        request("POST",execution_path+"/dispatch",execution_route+"/dispatch",body={**tool_cas(view),"acknowledge_cost":False},expected=400)
        view=request("POST",execution_path+"/dispatch",execution_route+"/dispatch",body={**tool_cas(view),"acknowledge_cost":True})["data"]
        expected="succeeded" if mode=="success" else "unknown";end=time.monotonic()+60
        while view["execution"]["status"]!=expected and time.monotonic()<end:
            assert view["execution"]["status"] in ("queued","leased","sent",expected)
            time.sleep(.1);view=request("GET",execution_path,execution_route)["data"]
        assert view["execution"]["status"]==expected
        if mode=="unknown":
            request("POST",execution_path+"/dispatch",execution_route+"/dispatch",body={**tool_cas(view),"acknowledge_cost":True},expected=409)
            view=request("GET",execution_path,execution_route)["data"]
            view=request("POST",execution_path+"/reconcile",execution_route+"/reconcile",body={"expected_task_version":view["task"]["version"],"expected_execution_version":view["execution"]["version"],"intent_hash":view["execution"]["intent_hash"],"outcome":"rejected","effect_reference":"local-fixture-observation","result":"","evidence_digest":hashlib.sha256(source_id.encode()).hexdigest(),"reason":"Synthetic operator assertion; no verified external fact or final bill."})["data"]
        exact=request("GET",source_path+"/artifacts/"+view["receipt"]["artifact_id"]+"?version=1","/tasks/{id}/artifacts/{artifact_id}")["data"]
        assert hashlib.sha256(exact["content"].encode()).hexdigest()==view["receipt"]["content_digest"]
        cost=request("GET",source_path+"/costs","/tasks/{id}/costs")["data"]
        current=request("GET",source_path,"/tasks/{id}")["data"]
        assert cost["spent_microusd"]=="2000" and cost["reserved_microusd"]=="17"
        assert current["status"]=="waiting_input" and current["candidate_id"]==source["candidate_id"] and current["current_run_id"]==source["current_run_id"] and not current.get("accepted_decision_id")
        tool_flows.append({"mode":mode,"task_id":source_id,"execution_id":view["execution"]["id"],"execution_status":view["execution"]["status"],"receipt_method":view["receipt"]["method"],"spent_microusd":cost["spent_microusd"],"reserved_microusd":cost["reserved_microusd"],"candidate_not_accepted":True,"billing_settlement":False})

    # Native proposals use installed public pins; each next inference is a new
    # reserved Operation in the same Run. This local profile is synthetic.
    native_model=next(m for m in models if m["model"]=="native-local-fixture")
    native_agent=request("POST","/api/v1/agents","/agents",body={"name":"Contract native fixture","instructions":"Propose registered synthetic tool, then read the exact observation.","model":native_model["model"],"tools":["tool_local_action_fixture"],"budget_microusd":10000},expected=201)["data"]
    assert len(native_agent["model_tools"])==1 and not {"endpoint","credential_environment","trusted_ca_pem"}.intersection(native_agent["model_tools"][0])
    native_agent=request("POST",f"/api/v1/agents/{native_agent['id']}/publish","/agents/{id}/publish",body={"expected_version":native_agent["version"]})["data"]
    native=request("POST","/api/v1/tasks","/tasks",body={"goal":"Contract native success fixture","agent_id":native_agent["id"],"budget_microusd":10000,"max_runs":1,"execution_timeout_seconds":600},expected=201)["data"]
    native_path=f"/api/v1/tasks/{native['id']}";end=time.monotonic()+60
    while native.get("waiting_reason")!="model_tools_pending" and time.monotonic()<end:
        assert native["status"] not in ("blocked","failed","cancelled")
        time.sleep(.1);native=request("GET",native_path,"/tasks/{id}")["data"]
    assert native.get("waiting_reason")=="model_tools_pending"
    source_run=native["current_run_id"];source_candidate=native["candidate_id"]
    assert request("GET",native_path+"/tool-executions","/tasks/{id}/tool-executions")["data"]==[]
    proposals_route="/tasks/{id}/model-tool-proposals";proposals_path=native_path+"/model-tool-proposals"
    proposals=request("GET",proposals_path+"?limit=1",proposals_route)["data"]
    assert len(proposals)==1 and "arguments_json" not in proposals[0]
    proposal=request("GET",proposals_path+"/"+proposals[0]["id"],proposals_route+"/{proposal_id}")["data"]
    assert proposal["candidate_id"]==source_candidate and "9007199254740993" in proposal["arguments_json"]
    request("POST",native_path+"/review","/tasks/{id}/review",body={"expected_version":native["version"],"candidate_id":source_candidate,"artifact_id":proposal["artifact_id"],"artifact_version":proposal["artifact_version"],"content_digest":proposal["content_digest"],"decision":"accept"},expected=409)
    source_operation=proposal["operation_id"]
    prepared={"model_proposal_id":proposal["id"],"occurrence_id":"model:"+proposal["id"],"expected_task_version":native["version"],"candidate_id":source_candidate,"artifact_id":proposal["artifact_id"],"artifact_version":proposal["artifact_version"],"content_digest":proposal["content_digest"],"tool_id":proposal["tool_id"],"tool_version":proposal["tool_version"],"arguments_json":proposal["arguments_json"],"expires_in_seconds":900}
    view=request("POST",native_path+"/tool-executions","/tasks/{id}/tool-executions",body=prepared,expected=201)["data"]
    execution_path=native_path+"/tool-executions/"+view["execution"]["id"];execution_route="/tasks/{id}/tool-executions/{execution_id}"
    view=request("POST",execution_path+"/decide",execution_route+"/decide",body={**tool_cas(view),"decision":"approve","reason":"Exact native synthetic proposal inspected"})["data"]
    assert view["execution"]["status"]=="approved" and not view.get("receipt")
    view=request("POST",execution_path+"/dispatch",execution_route+"/dispatch",body={**tool_cas(view),"acknowledge_cost":True})["data"]
    end=time.monotonic()+60
    while view["execution"]["status"]!="succeeded" and time.monotonic()<end:
        time.sleep(.1);view=request("GET",execution_path,execution_route)["data"]
    assert view["execution"]["status"]=="succeeded" and view["receipt"]["method"]=="registered_tool_response"
    native=request("GET",native_path,"/tasks/{id}")["data"]
    binding={"proposal_id":proposal["id"],"execution_id":view["execution"]["id"],"execution_version":view["execution"]["version"],"status":view["execution"]["status"],"receipt_id":view["receipt"]["id"],"content_digest":view["receipt"]["content_digest"]}
    continuing={"expected_task_version":native["version"],"candidate_id":source_candidate,"artifact_id":proposal["artifact_id"],"artifact_version":proposal["artifact_version"],"content_digest":proposal["content_digest"],"results":[binding],"acknowledge_cost":True,"reason":"Continue with exact inspected synthetic result"}
    native=request("POST",native_path+"/continue-model","/tasks/{id}/continue-model",body=continuing)["data"]
    assert native["current_run_id"]==source_run and native["attempt_count"]==1
    end=time.monotonic()+60
    while not (native["status"]=="waiting_input" and native.get("candidate_id") not in (None,source_candidate)) and time.monotonic()<end:
        assert native["status"] not in ("blocked","failed","cancelled")
        time.sleep(.1);native=request("GET",native_path,"/tasks/{id}")["data"]
    final_candidate=next(c for c in request("GET",native_path+"/candidates","/tasks/{id}/candidates")["data"] if c["id"]==native["candidate_id"])
    assert final_candidate["source"]=="model" and final_candidate["operation_id"]!=source_operation and final_candidate["run_id"]==source_run
    assert not native.get("accepted_decision_id")
    native=request("POST",native_path+"/review","/tasks/{id}/review",body={"expected_version":native["version"],"candidate_id":final_candidate["id"],"artifact_id":final_candidate["artifact_id"],"artifact_version":final_candidate["artifact_version"],"content_digest":final_candidate["content_digest"],"decision":"accept","reason":"Separate acceptance of synthetic final prose"})["data"]
    native_cost=request("GET",native_path+"/costs","/tasks/{id}/costs")["data"]
    assert native["status"]=="succeeded" and native_cost["spent_microusd"]=="0" and native_cost["reserved_microusd"]==str(2*native_model["max_charge_microusd"]+17)
    native_flow={"task_id":native["id"],"proposal_id":proposal["id"],"execution_id":view["execution"]["id"],"initial_operation_id":source_operation,"continued_operation_id":final_candidate["operation_id"],"run_id":source_run,"status":native["status"],"reserved_microusd":native_cost["reserved_microusd"],"spent_microusd":native_cost["spent_microusd"],"separate_final_acceptance":True,"billing_settlement":False}

    # Separate native task exercises bounded prepare-only delegation. No tool
    # is approved or dispatched; revocation preserves any prior model liability.
    automated=request("POST","/api/v1/tasks","/tasks",body={"goal":"Contract automation cleanup fixture","agent_id":native_agent["id"],"budget_microusd":10000},expected=201)["data"]
    auto_path="/api/v1/tasks/"+automated["id"]
    auto_deadline=time.monotonic()+30
    while automated["status"] not in {"waiting_input","waiting_approval"} and time.monotonic()<auto_deadline:
        assert automated["status"] not in {"blocked","failed","cancelled","succeeded"}
        time.sleep(.1);automated=request("GET",auto_path,"/tasks/{id}")["data"]
    assert automated["status"]=="waiting_input"
    auto_state=request("GET",auto_path+"/automation","/tasks/{id}/automation")["data"]
    pin=native_agent["model_tools"][0]
    automated=request("GET",auto_path,"/tasks/{id}")["data"]
    auto_state=request("POST",auto_path+"/automation","/tasks/{id}/automation",body={"expected_task_version":automated["version"],"mode":"prepare_and_continue","tool_pins":[{k:pin[k] for k in ("tool_id","tool_version","tool_profile_digest")}],"max_model_turns":2,"max_tool_executions":2,"max_charge_microusd":"5000","expires_in_seconds":900,"acknowledge_cost":True,"reason":"Explicit prepare-only synthetic contract check"},expected=201)["data"]
    auto_grant=auto_state["grant"]
    auto_state=request("POST",auto_path+"/automation/revoke","/tasks/{id}/automation/revoke",body={"grant_id":auto_grant["id"],"expected_version":auto_grant["version"],"reason":"Stop explicit contract delegation"})["data"]
    assert auto_state["grant"]["status"]=="revoked"
    request("GET",auto_path+"/progress-signals?limit=100","/tasks/{id}/progress-signals")
    request("POST",auto_path+"/cancel","/tasks/{id}/cancel",body={})

    # Wire decoding is stronger than structural Schema for duplicate JSON members
    # and lexical integer notation; both must still reject invalid mutations.
    malformed = [
        {**task_body, "budget_microusd": 0},
        {**task_body, "budget_microusd": None},
        {**task_body, "budget_microusd": 1.5},
        {**task_body, "unknown_capability": True},
    ]
    for candidate in malformed:
        request("POST", "/api/v1/tasks", "/tasks", body=candidate, expected=400)
    request("POST", "/api/v1/agents", "/agents", body={"name": "я" * 120, "instructions": "Fixture byte-limit check"}, expected=400)
    encoded = json.dumps(task_body)
    duplicate = encoded[:-1] + ',"budget_microusd":10000}'
    request("POST", "/api/v1/tasks", "/tasks", raw=duplicate.encode(), expected=400)
    lexical_float = encoded.replace('"budget_microusd": 10000', '"budget_microusd": 10000.0')
    request("POST", "/api/v1/tasks", "/tasks", raw=lexical_float.encode(), expected=400)

    report = {
        "contract_version": version, "result": "pass", "provider": "fixture:echo",
        "synthetic": True, "task_status": task["status"], "spent_microusd": task["spent_microusd"],
        "object_counts": object_counts, "responses": observations, "collaboration": collaboration, "materials": material_flow, "memberships":membership_flow, "tool_flows": tool_flows, "native_model_flow": native_flow,
        "method_path_count": len({(v["method"],v["route"]) for v in observations}),
        "limits": ["No real model or corporate IdP invoked", "Empty response collections prove only envelope/array shape", "No browser SSO or complete MCP implementation is claimed"],
    }
    assert report["method_path_count"] == 50
    args.report.parent.mkdir(parents=True, exist_ok=True)
    args.report.write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({k: report[k] for k in ("contract_version", "result", "provider", "synthetic", "task_status", "spent_microusd", "object_counts")}))


if __name__ == "__main__":
    main()
