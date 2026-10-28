"""Run both SDK examples against a live, authorized development platform.

This opt-in scenario is deliberately excluded from unittest discovery. It reads
credentials into memory, runs the actual example programs, and independently
checks their server-side tasks and artifacts. It exercises fixture:echo and
explicitly installed synthetic native-model/action/read peers, including
deliberate response loss. It creates persistent test records and reservations.
"""

from __future__ import annotations

import argparse
import ipaddress
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import uuid
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, ProxyHandler, build_opener

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "packages/python-sdk/src"))

from agent_platform import Client
from review_execution import exercise_review
from material_execution import exercise_materials
from tool_execution import exercise_tools
from model_loop_execution import exercise_model_loop
from automation_execution import exercise_automation


class NoRedirects(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise RuntimeError("SDK fixture probe refused redirect")


def inspect_execution(client: Client, task: dict) -> dict:
    """Check 0.8 read paths and independent cursor traversal on settled fixture work."""
    task_id = task["id"]
    assert task["root_id"] == task_id and task["depth"] == 0
    assert task["financial_hold"] is False
    identity=client.get_current_identity()
    assert identity["organization_id"]==task["organization_id"] and identity["principal_id"]
    counts = {}
    available_models = len(client.list_models(limit=100).data)
    for collection in ("runs", "operations", "actions", "approvals", "candidates", "decisions", "children"):
        ids: set[str] = set()
        cursors: set[str] = set()
        cursor = None
        while True:
            page = getattr(client, collection)(task_id, cursor=cursor, limit=1)
            for item in page.data:
                assert item.get("parent_id") == task_id if collection == "children" else item["task_id"] == task_id
                assert item["id"] not in ids, "Duplicate execution record across pages"
                ids.add(item["id"])
            cursor = page.next_cursor
            if cursor is None:
                break
            assert cursor not in cursors and len(cursors) < 100, "Bounded execution pagination"
            cursors.add(cursor)
        counts[collection] = len(ids)
    assert counts["runs"] and counts["operations"] and counts["candidates"]
    assert task["attempt_count"] == 1 and task["max_runs"] == 3 and task["current_run_id"]
    candidate = next(item for item in client.candidates(task_id).data if item["id"] == task["candidate_id"])
    assert candidate["source"] == "fixture" and candidate["run_id"] == task["current_run_id"]
    assert not task.get("accepted_decision_id"), "Fixture echo must not invent an authorized user decision"
    report = client.costs(task_id, limit=1)
    assert report["budget_microusd"] == task["budget_microusd"]
    assert report["spent_microusd"] == "1000" and report["reserved_microusd"] == "0"
    assert report["unknown_microusd"] == "0" and report["financial_hold"] is False
    assert report["pending_quarantine_count"] == 0 and report["incomplete"] is False
    for collection, cursor_name in (("reservations", "reservation_cursor"),
                                    ("entries", "entry_cursor"), ("incidents", "incident_cursor"),
                                    ("quarantines", "quarantine_cursor")):
        page = report
        ids = set()
        cursors = set()
        while True:
            for item in page[collection]:
                assert item["id"] not in ids, "Duplicate accounting record across pages"
                ids.add(item["id"])
                if collection == "entries":
                    assert item["synthetic"] is True
            cursor = page.get("next_" + cursor_name)
            if cursor is None:
                break
            assert cursor not in cursors and len(cursors) < 100, "Bounded cost pagination"
            cursors.add(cursor)
            page = client.costs(task_id, limit=1, **{cursor_name: cursor})
        counts[collection] = len(ids)
    assert counts["reservations"] and counts["entries"]
    return {"sdk": "python", "task_id": task_id, "counts": counts,
            "available_models": available_models,
            "budget_microusd": report["budget_microusd"],
            "synthetic_spent_microusd": report["spent_microusd"],
            "reserved_microusd": report["reserved_microusd"],
            "unknown_microusd": report["unknown_microusd"],
            "financial_hold": report["financial_hold"]}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default="http://127.0.0.1:8080")
    parser.add_argument("--credentials", type=Path, default=ROOT / ".local/bootstrap.json")
    parser.add_argument("--node", default="node", help="Node.js 24 executable")
    parser.add_argument("--wait-seconds", type=float, default=60.0)
    parser.add_argument("--preflight-only", action="store_true", help="Read-only local health/catalog check; do not create agents, tasks or a report")
    parser.add_argument("--tool-fixtures", type=Path, default=ROOT / "tests/fixtures/tool-scenarios.json")
    parser.add_argument("--report", type=Path, default=ROOT / ".local/qa/sdk-live-report.json")
    args = parser.parse_args()

    url = urlsplit(args.url)
    try:
        loopback = ipaddress.ip_address(url.hostname or "").is_loopback
    except ValueError:
        loopback = False
    if url.scheme != "http" or not loopback or url.username or url.password or url.path or url.query or url.fragment:
        parser.error("This development fixture probe accepts a literal loopback HTTP origin only")
    health_client = build_opener(ProxyHandler({}), NoRedirects())
    with health_client.open(args.url + "/healthz", timeout=10) as response:
        health = json.loads(response.read(65537))
    if health.get("data", {}).get("contract_version") != "0.14.0":
        parser.error("A running contract 0.14.0 server is required; no fixture was created")

    if not (ROOT / "packages/typescript-sdk/dist/index.js").is_file():
        parser.error("Build packages/typescript-sdk before running this scenario")
    fixtures = json.loads(args.tool_fixtures.read_text())
    assert set(fixtures) == {"success", "unknown"}
    assert all(set(value) == {"tool_id", "arguments_json"} and value["tool_id"] == "tool_local_action_fixture" and isinstance(value["arguments_json"], str) for value in fixtures.values()), "Require explicit installed local synthetic tool manifest"
    credentials = json.loads(args.credentials.read_text())
    token = credentials["token"]
    organization_id = credentials["organization_id"]
    environment = os.environ.copy()
    environment.update({
        "AGENT_PLATFORM_URL": args.url,
        "AGENT_PLATFORM_TOKEN": token,
        "AGENT_PLATFORM_ORGANIZATION_ID": organization_id,
        "PYTHONPATH": str(ROOT / "packages/python-sdk/src"),
    })
    client = Client(args.url, token=token, organization_id=organization_id)
    # Check all required installed fixtures before any agent/task is created.
    # Catalog metadata is not proof of real billing or arbitrary peer safety;
    # profiles must have been installed by the trusted local operator.
    models = client.list_models(limit=100).data
    tools = client.tools(limit=100).data
    if not any(m["model"] == "native-local-fixture" and m["max_charge_microusd"] == 1000 for m in models):
        parser.error("Install the explicit native model fixture before creating live SDK records")
    by_id = {t["id"]: t for t in tools}
    write, read = by_id.get("tool_local_action_fixture"), by_id.get("tool_local_read_fixture")
    if not write or not read or not write["active"] or not read["active"] or write["effect"] != "write" or read["effect"] != "read" or write["max_charge_microusd"] != "17" or read["max_charge_microusd"] != "9" or read.get("automation_allowed") is not True:
        parser.error("Install the explicit synthetic write and opt-in read fixtures before creating live SDK records")
    if args.preflight_only:
        print(json.dumps({"passed": True, "contract_version": "0.14.0", "scope": "local health and installed synthetic catalog only; peer connectivity, execution and billing not tested", "mutations": 0}))
        return 0
    args.report.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    run_id = "sdk-e2e-" + uuid.uuid4().hex
    report: dict = {"contract_version": "0.14.0", "run_id": run_id, "model": "fixture:echo", "synthetic_cost": True, "results": []}
    for language, command in (
        ("python", [sys.executable, "examples/python/fixture_task.py"]),
        ("typescript", [args.node, "examples/typescript/fixture_task.ts"]),
    ):
        command += ["--request-key", run_id + ":" + language,
                    "--wait-seconds", str(args.wait_seconds)]
        result = subprocess.run(
            command, cwd=ROOT, env=environment, text=True,
            capture_output=True, timeout=args.wait_seconds + 120,
        )
        if result.returncode:
            details = (result.stdout + result.stderr).replace(token, "<redacted>")
            raise RuntimeError(f"{language} example failed ({result.returncode}): {details}")
        match = re.search(r"^Task: ([A-Za-z0-9_-]+)$", result.stdout, re.MULTILINE)
        if not match:
            raise RuntimeError(f"{language} example did not report its task ID")
        task = client.get_task(match.group(1))
        artifacts = client.artifacts(task["id"]).data
        events = client.events(task["id"], after="0", limit=100).data
        if task["status"] != "succeeded":
            raise RuntimeError(f"{language} task ended with {task['status']}")
        if not artifacts or not all(artifact.get("content") for artifact in artifacts):
            raise RuntimeError(f"{language} task has no nonempty artifact")
        if not all(
            artifact["provenance"].get("adapter") == "fixture:echo"
            and artifact["provenance"].get("synthetic_cost") is True
            and artifact["provenance"].get("verified") is True
            and artifact["provenance"].get("verification") == "exact_goal_echo"
            for artifact in artifacts
        ):
            raise RuntimeError(f"{language} artifact is not a verified, explicitly synthetic fixture")
        if task["reserved_microusd"] != "0" or task["spent_microusd"] != "1000":
            raise RuntimeError(f"{language} fixture accounting did not settle at 1000 micro-USD")
        inspection = inspect_execution(client, task)
        ts_inspection = subprocess.run(
            [args.node, "tests/sdk/read_execution.ts", task["id"]],
            cwd=ROOT, env=environment, text=True, capture_output=True, timeout=60,
        )
        if ts_inspection.returncode:
            details = (ts_inspection.stdout + ts_inspection.stderr).replace(token, "<redacted>")
            raise RuntimeError(f"TypeScript execution inspection failed: {details}")
        ts_report = json.loads(ts_inspection.stdout)
        if (inspection["counts"] != ts_report["counts"]
                or inspection["available_models"] != ts_report["available_models"]):
            raise RuntimeError("SDKs disagree on settled fixture execution counts")
        report["results"].append({
            "sdk": language,
            "task_id": task["id"],
            "status": task["status"],
            "budget_microusd": task["budget_microusd"],
            "reserved_microusd": task["reserved_microusd"],
            "synthetic_spent_microusd": task["spent_microusd"],
            "event_types": [event["type"] for event in events],
            "execution_inspection": [inspection, ts_report],
            "artifacts": [{
                "id": artifact["id"],
                "name": artifact["name"],
                "media_type": artifact["media_type"],
                "version": artifact["version"],
                "content_bytes": len(artifact["content"].encode("utf-8")),
                "provenance": artifact["provenance"],
            } for artifact in artifacts],
        })
    report["review_flows"] = [exercise_review(client, run_id+":py-review", args.wait_seconds)]
    environment["FIXTURE_WAIT_SECONDS"] = str(args.wait_seconds)
    environment["AGENT_PLATFORM_TOOL_FIXTURES_JSON"] = json.dumps(fixtures)
    review_result = subprocess.run([args.node, "tests/sdk/review_execution.ts"], cwd=ROOT, env=environment, text=True, capture_output=True, timeout=args.wait_seconds * 2 + 120)
    if review_result.returncode:
        details = (review_result.stdout + review_result.stderr).replace(token, "<redacted>")
        raise RuntimeError("TypeScript review flow failed: " + details)
    report["review_flows"].append(json.loads(review_result.stdout))
    report["material_flows"] = [exercise_materials(client, run_id+":py-material", args.wait_seconds)]
    material_result = subprocess.run([args.node, "tests/sdk/material_execution.ts"], cwd=ROOT, env=environment, text=True, capture_output=True, timeout=args.wait_seconds + 120)
    if material_result.returncode:
        details = (material_result.stdout + material_result.stderr).replace(token, "<redacted>")
        raise RuntimeError("TypeScript material flow failed: " + details)
    report["material_flows"].append(json.loads(material_result.stdout))
    report["tool_flows"] = [exercise_tools(client, run_id+":py-tools", args.wait_seconds, fixtures)]
    tool_result = subprocess.run([args.node, "tests/sdk/tool_execution.ts"], cwd=ROOT, env=environment, text=True, capture_output=True, timeout=args.wait_seconds * 2 + 120)
    if tool_result.returncode:
        details = (tool_result.stdout + tool_result.stderr).replace(token, "<redacted>")
        raise RuntimeError("TypeScript tool flow failed: " + details)
    report["tool_flows"].append(json.loads(tool_result.stdout))
    report["model_loop_flows"] = [exercise_model_loop(client, run_id+":py-native", args.wait_seconds)]
    native_result = subprocess.run([args.node, "tests/sdk/model_loop_execution.ts"], cwd=ROOT, env=environment, text=True, capture_output=True, timeout=args.wait_seconds * 4 + 120)
    if native_result.returncode:
        details = (native_result.stdout + native_result.stderr).replace(token, "<redacted>")
        raise RuntimeError("TypeScript native model flow failed: " + details)
    report["model_loop_flows"].append(json.loads(native_result.stdout))
    report["automation_flows"] = [exercise_automation(client, run_id+":py-automation", args.wait_seconds)]
    automation_result = subprocess.run([args.node, "tests/sdk/automation_execution.ts"], cwd=ROOT, env=environment, text=True, capture_output=True, timeout=args.wait_seconds * 6 + 120)
    if automation_result.returncode:
        details = (automation_result.stdout + automation_result.stderr).replace(token, "<redacted>")
        raise RuntimeError("TypeScript automation flow failed: " + details)
    report["automation_flows"].append(json.loads(automation_result.stdout))
    args.report.write_text(json.dumps(report, ensure_ascii=False, indent=2)+"\n")
    print(json.dumps(report, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
