#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Independent HTTP boundary checks; standard library only.

Requires a running local fixture server and TWO distinct bootstrap credential
files created by the integrator. This script cannot check database races,
worker fencing, process-crash durability, SSO, real costs, or sandboxing.
It creates durable fixture records; use an isolated test database.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
import json
from pathlib import Path
import stat
import re
import sys
import time
import unittest
from urllib.error import HTTPError
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener
from uuid import uuid4


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


CONFIG = None
ROOT = Path(__file__).resolve().parents[2]
API_VERSION_HEADER = "X-Platform-API-Version"
API_VERSION = json.loads((ROOT / "contracts/openapi.json").read_text())["info"]["version"]
if not isinstance(API_VERSION, str) or not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", API_VERSION):
    raise RuntimeError("Invalid public contract version metadata")


def preflight(base_url):
    parsed = urlsplit(base_url)
    if (parsed.scheme != "http" or parsed.hostname not in {"127.0.0.1", "::1", "localhost"}
            or parsed.username or parsed.password or parsed.path not in ("", "/") or parsed.query or parsed.fragment):
        raise ValueError("Tests require a loopback HTTP origin without credentials")
    opener = build_opener(ProxyHandler({}), NoRedirect())
    with opener.open(base_url.rstrip("/") + "/healthz", timeout=10) as response:
        payload = response.read(65537)
        if len(payload) > 65536:
            raise RuntimeError("Health response exceeds preflight bound")
        health = json.loads(payload)
    if health.get("data", {}).get("contract_version") != API_VERSION:
        raise RuntimeError(f"Require API {API_VERSION}; no live fixture was created")
    return {"contract_version": API_VERSION, "mutations": 0,
            "scope": "health version only; credentials, worker and permissions not checked"}


def credentials(path):
    source = Path(path)
    if stat.S_IMODE(source.stat().st_mode) & 0o077:
        raise ValueError("Credential file must not be accessible to group/others")
    value = json.loads(source.read_text())
    if not all(isinstance(value.get(k), str) and value[k]
               for k in ("organization_id", "principal_id", "token")):
        raise ValueError("Invalid bootstrap credentials")
    return value


class HTTPBoundaryTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.base = CONFIG.base_url.rstrip("/")
        parsed = urlsplit(cls.base)
        if (parsed.scheme != "http" or parsed.hostname not in
                {"127.0.0.1", "::1", "localhost"} or parsed.username or parsed.password):
            raise ValueError("Tests require a loopback HTTP URL without credentials")
        preflight(cls.base)
        cls.a = credentials(CONFIG.org_a)
        cls.b = credentials(CONFIG.org_b)
        if cls.a["organization_id"] == cls.b["organization_id"]:
            raise ValueError("Two distinct organizations are required")
        cls.prefix = "security-" + uuid4().hex

    def request(self, method, path, identity=None, body=None, key=None,
                extra_headers=None, raw=None):
        headers = {"Content-Type": "application/json", API_VERSION_HEADER: API_VERSION}
        if identity:
            headers["Authorization"] = "Bearer " + identity["token"]
            headers["X-Organization-ID"] = identity["organization_id"]
        if key:
            headers["Idempotency-Key"] = key
        headers.update(extra_headers or {})
        data = raw if raw is not None else (
            json.dumps(body).encode() if body is not None else None)
        req = Request(self.base + path, data=data, headers=headers, method=method)
        opener = build_opener(ProxyHandler({}), NoRedirect())
        try:
            response = opener.open(req, timeout=15)
        except HTTPError as exc:
            response = exc
        with response:
            status = response.status
            if path.startswith("/api/v1/") and response.headers.get_all(API_VERSION_HEADER, []) != [API_VERSION]:
                raise RuntimeError("Incompatible or ambiguous API response version")
            response_headers = dict(response.headers.items())
            content = response.read(2 << 20)
        try:
            payload = json.loads(content)
        except (UnicodeDecodeError, json.JSONDecodeError):
            self.fail("Expected JSON response, got HTTP %d" % status)
        return status, payload, response_headers

    def agent(self, identity, publish=True):
        code, response, _ = self.request("POST", "/api/v1/agents", identity,
            {"name": self.prefix, "instructions": "Deterministic security fixture",
             "model": "fixture:echo", "budget_microusd": 10000}, uuid4().hex)
        self.assertEqual(code, 201)
        agent = response["data"]
        if publish:
            code, _, _ = self.request("POST", "/api/v1/agents/" + agent["id"] + "/publish",
                identity, {"expected_version": agent["version"]}, uuid4().hex)
            self.assertEqual(code, 200)
        return agent

    def task(self, identity, agent=None, goal=None, budget=1000, key=None):
        agent = agent or self.agent(identity)
        body = {"goal": goal or self.prefix, "agent_id": agent["id"],
                "budget_microusd": budget}
        return self.request("POST", "/api/v1/tasks", identity, body, key or uuid4().hex)

    def test_01_authentication_and_header_spoofing(self):
        """SEC-01: identity is not accepted from client-selected headers."""
        for identity, headers in [
            (None, {}),
            (None, {"X-Organization-ID": self.a["organization_id"],
                    "X-User-ID": self.a["principal_id"], "X-Role": "owner"}),
            (self.a, {"Authorization": "Bearer invalid-security-test-token"}),
            (self.a, {"X-Organization-ID": self.b["organization_id"]}),
        ]:
            with self.subTest(case=bool(identity), header_names=list(headers)):
                code, response, _ = self.request("GET", "/api/v1/tasks", identity,
                                                 extra_headers=headers)
                self.assertEqual(code, 401)
                self.assertEqual(response["error"]["code"], "unauthenticated")

    def test_02_foreign_task_and_nested_resources(self):
        """SEC-02: foreign IDs cannot read/cancel task or fetch events/artifacts."""
        code, response, _ = self.task(self.b)
        self.assertEqual(code, 201)
        path = "/api/v1/tasks/" + response["data"]["id"]
        for suffix in ("", "/events", "/artifacts"):
            code, body, _ = self.request("GET", path + suffix, self.a)
            self.assertEqual(code, 404)
            self.assertEqual(body["error"]["code"], "not_found")
        code, _, _ = self.request("POST", path + "/cancel", self.a, {}, uuid4().hex)
        self.assertEqual(code, 404)

    def test_03_foreign_agent_and_injected_ownership(self):
        """SEC-03/04: foreign references and client-set ownership are rejected."""
        foreign = self.agent(self.b)
        code, _, _ = self.task(self.a, foreign)
        self.assertEqual(code, 404)
        own = self.agent(self.a)
        valid = {"goal": self.prefix, "agent_id": own["id"], "budget_microusd": 1000}
        for field, value in (("organization_id", self.b["organization_id"]),
                             ("owner_id", self.b["principal_id"]),
                             ("role", "owner"), ("status", "succeeded")):
            code, _, _ = self.request("POST", "/api/v1/tasks", self.a,
                dict(valid, **{field: value}), uuid4().hex)
            self.assertEqual(code, 400)

    def test_04_idempotency_concurrent_same_payload(self):
        """SEC-07: simultaneous duplicate HTTP commands return one task ID."""
        agent = self.agent(self.a)
        key = uuid4().hex
        goal = self.prefix + " concurrent " + key
        with ThreadPoolExecutor(max_workers=8) as pool:
            replies = list(pool.map(lambda _: self.task(self.a, agent, goal, key=key), range(16)))
        self.assertEqual({r[0] for r in replies}, {201})
        self.assertEqual(len({r[1]["data"]["id"] for r in replies}), 1)
        self.assertEqual(len({json.dumps(r[1]["data"], sort_keys=True) for r in replies}), 1)

    def test_05_idempotency_payload_conflict_and_tenant_scope(self):
        """SEC-07: conflicting payload fails and a tenant cannot replay another's result."""
        key = uuid4().hex
        agent_a = self.agent(self.a)
        code, first, _ = self.task(self.a, agent_a, goal="original " + key, key=key)
        self.assertEqual(code, 201)
        code, body, _ = self.task(self.a, agent_a, goal="changed " + key, key=key)
        self.assertEqual(code, 409)
        self.assertEqual(body["error"]["code"], "conflict")
        code, second, _ = self.task(self.b, goal="original " + key, key=key)
        self.assertEqual(code, 201)
        self.assertNotEqual(first["data"]["id"], second["data"]["id"])
        self.assertEqual(second["data"]["organization_id"], self.b["organization_id"])

    def test_06_draft_agent_and_stale_publish(self):
        draft = self.agent(self.a, publish=False)
        code, _, _ = self.task(self.a, draft)
        self.assertEqual(code, 404)
        code, _, _ = self.request("POST", "/api/v1/agents/" + draft["id"] + "/publish",
            self.a, {"expected_version": draft["version"] + 1}, uuid4().hex)
        self.assertEqual(code, 409)

    def test_07_money_types_bounds_and_agent_cap(self):
        """SEC-15: request money is integer, positive and within the agent cap."""
        agent = self.agent(self.a)
        for amount in (-1, 0, 1.5, "1000", 2 ** 64, 10 ** 12 + 1):
            with self.subTest(amount=str(amount)):
                code, _, _ = self.task(self.a, agent, budget=amount)
                self.assertEqual(code, 400)
        code, body, _ = self.task(self.a, agent, budget=10001)
        self.assertEqual(code, 409)
        self.assertEqual(body["error"]["code"], "budget_exceeded")

    def test_08_request_and_query_limits(self):
        """SEC-17/19: bounded parsing and pagination; not a load/queue-limit test."""
        for query in ("limit=0", "limit=101", "limit=-1", "cursor=not-valid-json"):
            code, _, _ = self.request("GET", "/api/v1/tasks?" + query, self.a)
            self.assertEqual(code, 400)
        code, _, _ = self.request("POST", "/api/v1/agents", self.a,
            raw=json.dumps({"name": "large", "instructions": "x" * (1 << 20)}).encode(),
            key=uuid4().hex)
        self.assertIn(code, (400, 413))
        code, _, _ = self.request("POST", "/api/v1/agents", self.a,
            raw=b'{} {}', key=uuid4().hex)
        self.assertEqual(code, 400)

    def test_09_listing_and_error_metadata(self):
        """SEC-02: listing and errors do not disclose cross-tenant resources."""
        for endpoint in ("tasks", "agents"):
            code, body, headers = self.request("GET", "/api/v1/" + endpoint, self.a)
            self.assertEqual(code, 200)
            self.assertTrue(all(row["organization_id"] == self.a["organization_id"]
                                for row in body["data"]))
            self.assertEqual(headers.get("Cache-Control"), "no-store")
        code, body, _ = self.request("GET", "/api/v1/tasks/nonexistent", self.a)
        self.assertEqual(code, 404)
        self.assertIn("request_id", body["error"])
        self.assertNotIn("token", json.dumps(body).lower())

    def test_10_cancel_idempotency(self):
        code, reply, _ = self.task(self.a)
        self.assertEqual(code, 201)
        path = "/api/v1/tasks/" + reply["data"]["id"] + "/cancel"
        key = uuid4().hex
        code, original, _ = self.request("POST", path, self.a, {}, key)
        self.assertEqual(code, 200)
        code, repeated, _ = self.request("POST", path, self.a, {}, key)
        self.assertEqual(code, 200)
        self.assertEqual(original, repeated)
        self.assertIn(original["data"]["status"], {"cancelled", "succeeded", "failed"})

    def test_11_fixture_result_and_synthetic_cost(self):
        """SEC-20 limited to normal HTTP result; malicious-worker test is separate."""
        if not CONFIG.with_worker:
            self.skipTest("Worker execution not requested; no result-verification claim")
        goal = self.prefix + " exact <script>fixture</script> " + uuid4().hex
        code, reply, _ = self.task(self.a, goal=goal)
        self.assertEqual(code, 201)
        task_id = reply["data"]["id"]
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            code, reply, _ = self.request("GET", "/api/v1/tasks/" + task_id, self.a)
            self.assertEqual(code, 200)
            if reply["data"]["status"] in {"succeeded", "failed", "blocked", "cancelled"}:
                break
            time.sleep(0.05)
        task = reply["data"]
        self.assertEqual(task["status"], "succeeded")
        self.assertEqual(task["result"], "Тестовый результат\n" + goal)
        # Contract 0.3 factual money is an exact decimal string. Integrator
        # adaptation preserves the original exact synthetic-cost assertions.
        self.assertEqual(task["spent_microusd"], "1000")
        self.assertEqual(task["reserved_microusd"], "0")
        code, artifacts, _ = self.request("GET", "/api/v1/tasks/" + task_id + "/artifacts", self.a)
        self.assertEqual(code, 200)
        self.assertEqual(len(artifacts["data"]), 1)
        self.assertEqual(artifacts["data"][0]["content"], task["result"])
        self.assertIs(artifacts["data"][0]["provenance"]["synthetic_cost"], True)
        self.assertEqual(artifacts["data"][0]["provenance"]["verification"], "exact_goal_echo")

    def test_12_unstorable_text_is_client_error(self):
        """CORE-04: PostgreSQL text cannot store U+0000; reject before SQL."""
        valid_agent = {"name": self.prefix, "instructions": "Security fixture"}
        for field in ("name", "instructions"):
            with self.subTest(field=field):
                body = dict(valid_agent, **{field: "untrusted\u0000text"})
                code, response, _ = self.request("POST", "/api/v1/agents", self.a,
                    body, uuid4().hex)
                self.assertEqual(code, 400)
                self.assertEqual(response["error"]["code"], "invalid_request")
        agent = self.agent(self.a)
        valid_task = {"goal": self.prefix, "agent_id": agent["id"], "budget_microusd": 1000}
        for field in ("goal", "agent_id"):
            with self.subTest(field=field):
                code, response, _ = self.request("POST", "/api/v1/tasks", self.a,
                    dict(valid_task, **{field: "untrusted\u0000text"}), uuid4().hex)
                self.assertEqual(code, 400)
                self.assertEqual(response["error"]["code"], "invalid_request")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default="http://127.0.0.1:8080")
    parser.add_argument("--org-a", help="Private bootstrap JSON for test tenant A")
    parser.add_argument("--org-b", help="Private bootstrap JSON for test tenant B")
    parser.add_argument("--with-worker", action="store_true")
    parser.add_argument("--preflight-only", action="store_true")
    CONFIG = parser.parse_args()
    if CONFIG.preflight_only:
        print(json.dumps(preflight(CONFIG.base_url)))
        sys.exit(0)
    if not CONFIG.org_a or not CONFIG.org_b:
        parser.error("--org-a and --org-b are required unless --preflight-only")
    unittest.main(argv=[__file__, "-v"])
