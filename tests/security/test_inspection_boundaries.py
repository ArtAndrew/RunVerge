#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Independent HTTP checks of inspection, Python stdlib only.

Independent HTTP inspection assertions with an explicit current contract gate.
The four collections and all existing tenant, cursor, authentication and
no-billing-write-endpoint assertions are unchanged.

No database access or direct financial writes. Without a fixture manifest this
creates one fixture:echo task per organization and waits for synthetic fixture
accounting by the local worker; it does not exercise paid model execution. Rich
stable fixtures can be supplied by the integrator to exercise all list cursors.
Never treats an empty/single-row collection as evidence of multi-page coverage.
"""
import argparse
import base64
import datetime
import hashlib
import json
from pathlib import Path
import time
import unittest
from urllib.parse import urlencode

import test_http_boundaries as foundation

CONFIG = None
KINDS = ("runs", "operations", "actions", "approvals", "costs")
COST_COLLECTIONS = {
    "reservations": ("reservation_cursor", "next_reservation_cursor"),
    "entries": ("entry_cursor", "next_entry_cursor"),
    "incidents": ("incident_cursor", "next_incident_cursor"),
    "quarantines": ("quarantine_cursor", "next_quarantine_cursor"),
}


class InspectionBoundaryTests(unittest.TestCase):
    request = foundation.HTTPBoundaryTests.request
    agent = foundation.HTTPBoundaryTests.agent
    task = foundation.HTTPBoundaryTests.task
    pagination_coverage = {}

    @classmethod
    def setUpClass(cls):
        foundation.CONFIG = CONFIG
        foundation.HTTPBoundaryTests.setUpClass.__func__(cls)
        client = cls("runTest")
        status, body, _ = client.request("GET", "/healthz")
        if status != 200 or body.get("data", {}).get("contract_version") != foundation.API_VERSION:
            raise RuntimeError(f"Require API {foundation.API_VERSION}; inspection tests not run")
        if CONFIG.fixture_manifest:
            fixtures = json.loads(Path(CONFIG.fixture_manifest).read_text())
            cls.task_a = fixtures["organization_a_task_id"]
            cls.task_b = fixtures["organization_b_task_id"]
        else:
            ids = []
            for identity in (cls.a, cls.b):
                status, body, _ = client.task(identity)
                if status != 201:
                    raise RuntimeError("Could not create isolated inspection fixture task")
                ids.append(body["data"]["id"])
            cls.task_a, cls.task_b = ids
            deadline = time.monotonic() + CONFIG.worker_timeout
            for identity, task_id in ((cls.a, cls.task_a), (cls.b, cls.task_b)):
                while True:
                    status, body, _ = client.request("GET", "/api/v1/tasks/" + task_id, identity)
                    state = body.get("data", {}).get("status")
                    if status == 200 and state == "succeeded":
                        break
                    if state in {"cancelled", "blocked", "failed"} or time.monotonic() >= deadline:
                        raise RuntimeError("Fixture worker did not produce a stable succeeded task")
                    time.sleep(0.1)
        for identity, task_id in ((cls.a, cls.task_a), (cls.b, cls.task_b)):
            if not isinstance(task_id, str) or "/" in task_id or "?" in task_id:
                raise ValueError("Invalid task identifier in inspection fixture manifest")
            status, body, _ = client.request("GET", "/api/v1/tasks/" + task_id, identity)
            if status != 200 or body["data"]["organization_id"] != identity["organization_id"]:
                raise RuntimeError("Fixture does not belong to its configured organization")

    def path(self, kind, task_id=None, **query):
        result = "/api/v1/tasks/" + (task_id or self.task_a) + "/" + kind
        return result + ("?" + urlencode(query) if query else "")

    def assert_safe_response(self, response, headers):
        serialized = json.dumps(response)
        for identity in (self.a, self.b):
            self.assertTrue(identity["token"] not in serialized, "credential disclosed")
        self.assertEqual(headers.get("Cache-Control"), "no-store")
        self.assertTrue(bool(headers.get("X-Request-Id") or headers.get("X-Request-ID")), "request ID absent")

    def get_collection(self, kind, collection=None, **query):
        status, body, headers = self.request("GET", self.path(kind, **query), self.a)
        self.assertEqual(status, 200)
        self.assert_safe_response(body, headers)
        data = body["data"]
        if collection:
            self.assertIsInstance(data, dict)
            rows = data[collection]
        else:
            rows = data
        self.assertIsInstance(rows, list)
        for row in rows:
            self.assertEqual(row["organization_id"], self.a["organization_id"])
            if kind != "costs":
                self.assertEqual(row["task_id"], self.task_a)
            elif collection == "reservations":
                self.assertIn(self.task_a, row["scope_task_ids"])
        return rows, data if collection else body

    def test_01_all_inspection_routes_require_authentication(self):
        for kind in KINDS:
            for identity, headers in ((None, {}), (self.a, {"X-Organization-ID": self.b["organization_id"]}),
                                      (self.a, {"Authorization": "Bearer invalid-inspection-token"})):
                with self.subTest(kind=kind, authenticated=identity is not None):
                    status, body, response_headers = self.request("GET", self.path(kind), identity, extra_headers=headers)
                    self.assertEqual(status, 401)
                    self.assertEqual(body["error"]["code"], "unauthenticated")
                    self.assert_safe_response(body, response_headers)

    def test_02_foreign_and_unknown_tasks_are_not_found(self):
        for kind in KINDS:
            for identity, task_id in ((self.a, self.task_b), (self.b, self.task_a), (self.a, "tsk_nonexistent_inspection")):
                with self.subTest(kind=kind, foreign=task_id != "tsk_nonexistent_inspection"):
                    status, body, headers = self.request("GET", self.path(kind, task_id), identity)
                    self.assertEqual(status, 404)
                    self.assertEqual(body["error"]["code"], "not_found")
                    self.assertNotIn("data", body)
                    self.assert_safe_response(body, headers)

    def test_03_limits_and_malformed_cursors_are_rejected(self):
        invalid_cursors = ("not-valid-json", "x" * 513,
            base64.urlsafe_b64encode(b'{"created_at":"2026-01-01T00:00:00Z","id":"x\\u0000y"}').decode().rstrip("="),
            base64.urlsafe_b64encode(b'{"id":"x"}').decode().rstrip("="),
            base64.urlsafe_b64encode(b'{}').decode().rstrip("="),
            base64.urlsafe_b64encode(b'null').decode().rstrip("="),
            base64.urlsafe_b64encode(b'{"created_at":"2026-01-01T00:00:00Z"}').decode().rstrip("="))
        for kind in KINDS:
            for value in ("0", "101", "-1", "1.5", "NaN", "999999999999999999999999"):
                with self.subTest(kind=kind, limit=value):
                    status, _, _ = self.request("GET", self.path(kind, limit=value), self.a)
                    self.assertEqual(status, 400)
            cursor_names = [v[0] for v in COST_COLLECTIONS.values()] if kind == "costs" else ["cursor"]
            for name in cursor_names:
                for case, value in enumerate(invalid_cursors):
                    with self.subTest(kind=kind, cursor=name, case=case):
                        status, _, _ = self.request("GET", self.path(kind, **{name: value}), self.a)
                        self.assertEqual(status, 400)

    def test_04_execution_pagination_is_bounded_without_duplicates(self):
        for kind in KINDS[:-1]:
            with self.subTest(kind=kind):
                baseline, envelope = self.get_collection(kind, limit=100)
                self.assertNotIn("next_cursor", envelope, "Fixture exceeds bounded independent test coverage")
                expected = [row["id"] for row in baseline]
                actual, seen_cursors, cursor = [], set(), ""
                for _ in range(101):
                    rows, page = self.get_collection(kind, limit=1, **({"cursor": cursor} if cursor else {}))
                    self.assertLessEqual(len(rows), 1)
                    actual.extend(row["id"] for row in rows)
                    cursor = page.get("next_cursor", "")
                    if not cursor:
                        break
                    self.assertNotIn(cursor, seen_cursors, "Cursor cycle")
                    seen_cursors.add(cursor)
                else:
                    self.fail("Pagination exceeded bounded page count")
                self.assertEqual(actual, expected)
                self.assertEqual(len(actual), len(set(actual)))
                self.pagination_coverage[kind] = {"rows": len(actual), "multiple_pages_exercised": len(actual) > 1}

    def test_05_cost_collection_cursors_are_independent(self):
        for collection, (query_name, output_name) in COST_COLLECTIONS.items():
            with self.subTest(collection=collection):
                baseline, report = self.get_collection("costs", collection, limit=100)
                self.assertNotIn(output_name, report, "Fixture exceeds bounded independent test coverage")
                expected = [row["id"] for row in baseline]
                totals = {name: report[name] for name in ("budget_microusd", "reserved_microusd", "spent_microusd", "unknown_microusd", "financial_hold")}
                actual, seen_cursors, cursor = [], set(), ""
                for _ in range(101):
                    rows, page = self.get_collection("costs", collection, limit=1, **({query_name: cursor} if cursor else {}))
                    self.assertLessEqual(len(rows), 1)
                    self.assertEqual({name: page[name] for name in totals}, totals)
                    for name in COST_COLLECTIONS:
                        self.assertLessEqual(len(page[name]), 1)
                    actual.extend(row["id"] for row in rows)
                    cursor = page.get(output_name, "")
                    if not cursor:
                        break
                    self.assertNotIn(cursor, seen_cursors, "Cursor cycle")
                    seen_cursors.add(cursor)
                else:
                    self.fail("Pagination exceeded bounded page count")
                self.assertEqual(actual, expected)
                self.assertEqual(len(actual), len(set(actual)))
                self.pagination_coverage["costs." + collection] = {"rows": len(actual), "multiple_pages_exercised": len(actual) > 1}

    def test_06_cursor_cannot_select_a_foreign_scope(self):
        # Cursors are positions, not authority. Even a deliberately constructed
        # cursor with a foreign ID must only return the selected task/org scope.
        forged = base64.urlsafe_b64encode(json.dumps({"created_at": "9999-01-01T00:00:00Z", "id": self.task_b}).encode()).decode().rstrip("=")
        for kind in KINDS[:-1]:
            rows, _ = self.get_collection(kind, cursor=forged, limit=100)
            self.assertTrue(all(r["task_id"] == self.task_a for r in rows))
        for collection, (query_name, _) in COST_COLLECTIONS.items():
            self.get_collection("costs", collection, limit=100, **{query_name: forged})

    def test_07_inspection_does_not_create_a_billing_write_endpoint(self):
        for kind in KINDS:
            status, body, headers = self.request("POST", self.path(kind), self.a,
                body={"actual_microusd": 0, "final": True, "currency": "USD"}, key="inspection-must-not-write")
            self.assertIn(status, (404, 405))
            self.assertNotIn("data", body)
            self.assert_safe_response(body, headers)


def main():
    global CONFIG
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default="http://127.0.0.1:8080")
    parser.add_argument("--org-a")
    parser.add_argument("--org-b")
    parser.add_argument("--fixture-manifest")
    parser.add_argument("--worker-timeout", type=float, default=45)
    parser.add_argument("--evidence")
    parser.add_argument("--preflight-only", action="store_true")
    CONFIG = parser.parse_args()
    if CONFIG.preflight_only:
        print(json.dumps(foundation.preflight(CONFIG.base_url)))
        return
    if not CONFIG.org_a or not CONFIG.org_b:
        parser.error("--org-a and --org-b are required unless --preflight-only")
    runner = unittest.TextTestRunner(verbosity=2)
    result = runner.run(unittest.defaultTestLoader.loadTestsFromTestCase(InspectionBoundaryTests))
    if CONFIG.evidence:
        evidence = {"date_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
            "suite": "Inspection HTTP boundaries", "contract_version": foundation.API_VERSION, "tests_run": result.testsRun,
            "failures": len(result.failures), "errors": len(result.errors), "skipped": len(result.skipped),
            "successful": result.wasSuccessful(), "fixture_mode": "integrator_seed" if CONFIG.fixture_manifest else "public_fixture_worker",
            "pagination_coverage": InspectionBoundaryTests.pagination_coverage,
            "runner_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
            "limitations": ["HTTP boundary checks only; no database race or external provider accounting verification.",
                "Collections with fewer than two rows do not prove multi-page traversal.",
                "Per-response consistency only; pagination does not claim one snapshot across concurrent mutations."]}
        Path(CONFIG.evidence).write_text(json.dumps(evidence, indent=2) + "\n")
    raise SystemExit(0 if result.wasSuccessful() else 1)


if __name__ == "__main__":
    main()
