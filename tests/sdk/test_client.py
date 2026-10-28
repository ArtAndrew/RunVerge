"""SDK contract tests against an in-process HTTP server; no external services."""

from __future__ import annotations

import json
import queue
import socket
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlsplit

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "packages/python-sdk/src"))

from agent_platform import APIError, Client, ProtocolError, TransportError

EXECUTION = json.loads(Path(__file__).with_name("execution-fixtures.json").read_text())


class ContractServer(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self) -> None:
        super().__init__(("127.0.0.1", 0), ContractHandler)
        self.responses: queue.Queue = queue.Queue()
        self.requests: queue.Queue = queue.Queue()


class ContractHandler(BaseHTTPRequestHandler):
    def do_GET(self) -> None:
        self.handle_request()

    def do_POST(self) -> None:
        self.handle_request()

    def handle_request(self) -> None:
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        self.server.requests.put({
            "method": self.command,
            "path": self.path,
            "headers": self.headers,
            "body": json.loads(body) if body else None,
        })
        status, data, headers = self.server.responses.get(timeout=2)
        if status == 0:
            self.connection.shutdown(socket.SHUT_RDWR)
            self.connection.close()
            return
        raw = data if isinstance(data, bytes) else json.dumps(data, ensure_ascii=False).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        if not any(key.lower() == "x-platform-api-version" for key in headers):
            self.send_header("X-Platform-API-Version", "0.14.0")
        self.send_header("Content-Length", str(len(raw)))
        for key, value in headers.items():
            self.send_header(key, value)
        self.end_headers()
        self.wfile.write(raw)

    def log_message(self, format: str, *args: object) -> None:
        pass


class ClientHTTPTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.server = ContractServer()
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()
        cls.base_url = f"http://127.0.0.1:{cls.server.server_port}"

    @classmethod
    def tearDownClass(cls) -> None:
        cls.server.shutdown()
        cls.server.server_close()
        cls.thread.join(timeout=2)

    def setUp(self) -> None:
        self.client = Client(self.base_url, token="private-test-token", organization_id="org-1")

    def tearDown(self) -> None:
        self.assertTrue(self.server.responses.empty(), "Unused response fixture")
        self.assertTrue(self.server.requests.empty(), "Unexpected or unasserted HTTP request")

    def respond(self, data: object, status: int = 200, **headers: str) -> None:
        self.server.responses.put((status, data, headers))

    def request(self) -> dict:
        return self.server.requests.get(timeout=2)

    def test_create_task_sends_contract_headers_and_integer_budget(self) -> None:
        self.respond({"data": {"id": "task-1", "reserved_microusd": "0", "spent_microusd": "0", "goal": "Подготовить отчёт", "extra": 1}}, 201)
        task = self.client.create_task("Подготовить отчёт", "agent-1", 250_000,
                                       idempotency_key="create-task-1")
        self.assertEqual(task["id"], "task-1")
        self.assertEqual(task["extra"], 1)
        request = self.request()
        self.assertEqual((request["method"], request["path"]), ("POST", "/api/v1/tasks"))
        self.assertEqual(request["body"], {
            "goal": "Подготовить отчёт", "agent_id": "agent-1", "budget_microusd": 250_000,
        })
        self.assertEqual(request["headers"]["Authorization"], "Bearer private-test-token")
        self.assertEqual(request["headers"]["X-Organization-ID"], "org-1")
        self.assertEqual(request["headers"]["Idempotency-Key"], "create-task-1")
        self.assertEqual(request["headers"]["Content-Type"], "application/json")
        self.assertNotIn("private-test-token", repr(self.client))

    def test_list_pagination_round_trips_an_opaque_cursor(self) -> None:
        cursor = "created:123+456/789=="
        self.respond({"data": [{"id": "task-2", "reserved_microusd": "0", "spent_microusd": "0"}], "next_cursor": cursor})
        page = self.client.list_tasks()
        self.assertEqual(page.next_cursor, cursor)
        self.assertEqual(page.data[0]["id"], "task-2")
        self.assertEqual(self.request()["path"], "/api/v1/tasks")
        self.respond({"data": []})
        empty = self.client.list_tasks(cursor=page.next_cursor)
        self.assertIsNone(empty.next_cursor)
        self.assertEqual(parse_qs(urlsplit(self.request()["path"]).query), {"cursor": [cursor]})

    def test_get_cancel_publish_and_artifact_paths(self) -> None:
        operations = [
            (lambda: self.client.get_task("task 1"), "GET", "/api/v1/tasks/task%201", None, {"id": "task 1", "reserved_microusd": "0", "spent_microusd": "0"}),
            (lambda: self.client.cancel_task("task-1", idempotency_key="cancel-1"), "POST", "/api/v1/tasks/task-1/cancel", "cancel-1", {"id": "task-1", "reserved_microusd": "0", "spent_microusd": "0"}),
            (lambda: self.client.publish_agent("agent-1", expected_version=2, idempotency_key="publish-1"), "POST", "/api/v1/agents/agent-1/publish", "publish-1", {"id": "agent-1"}),
            (lambda: self.client.artifacts("task-1"), "GET", "/api/v1/tasks/task-1/artifacts", None, [{"id": "artifact-1"}]),
        ]
        for operation, method, path, key, result in operations:
            with self.subTest(path=path):
                self.respond({"data": result})
                operation()
                request = self.request()
                self.assertEqual((request["method"], request["path"]), (method, path))
                self.assertEqual(request["headers"].get("Idempotency-Key"), key)
                if path.endswith("/publish"):
                    self.assertEqual(request["body"], {"expected_version": 2})

    def test_agent_definition_is_sent_without_transformation(self) -> None:
        definition = {"name": "Research", "instructions": "Cite sources", "model": "fixture:echo", "tools": [], "budget_microusd": 100}
        self.respond({"data": {"id": "agent-1", **definition}}, 201)
        agent = self.client.create_agent(**definition, idempotency_key="new-agent-1")
        self.assertEqual(agent["name"], "Research")
        self.assertEqual(self.request()["body"], definition)
        self.respond({"data": [agent]})
        self.assertEqual(self.client.list_agents().data[0]["id"], "agent-1")
        self.assertEqual(self.request()["path"], "/api/v1/agents")

    def test_events_accept_reconnect_cursor(self) -> None:
        self.respond({"data": [{"id": "event-9", "organization_id":"org-1", "task_id":"task-1", "sequence": "9", "type": "task.completed", "payload_version":0, "legacy_data_json":"{\"wide\":9007199254740993}", "created_at":"2026-10-24T00:00:00Z"}]})
        events = self.client.events("task-1", after="8", limit=50)
        self.assertEqual(events.data[0]["sequence"], "9")
        request = self.request()
        self.assertEqual(request["path"], "/api/v1/tasks/task-1/events?after=8&limit=50")
        self.assertEqual(request["headers"]["Accept"], "application/json")

    def test_execution_pages_preserve_opaque_cursors_and_authorization(self) -> None:
        cursor = "123+456/789=="
        for collection in ("runs", "operations", "actions", "approvals"):
            method = getattr(self.client, collection)
            with self.subTest(collection=collection):
                self.respond({"data": [EXECUTION[collection]], "next_cursor": cursor})
                page = method("task 1", limit=1)
                self.assertEqual(page.data, [EXECUTION[collection]])
                self.assertEqual(page.next_cursor, cursor)
                request = self.request()
                self.assertEqual(request["method"], "GET")
                self.assertEqual(request["path"], f"/api/v1/tasks/task%201/{collection}?limit=1")
                self.assertEqual(request["headers"]["Authorization"], "Bearer private-test-token")
                self.assertEqual(request["headers"]["X-Organization-ID"], "org-1")
                self.assertIsNone(request["headers"].get("Idempotency-Key"))
                self.assertIsNone(request["body"])
                self.respond({"data": []})
                self.assertIsNone(method("task-1", cursor=page.next_cursor).next_cursor)
                self.assertEqual(parse_qs(urlsplit(self.request()["path"]).query), {"cursor": [cursor]})

    def test_costs_preserve_totals_signed_deltas_and_independent_cursors(self) -> None:
        self.respond({"data": EXECUTION["costs"]})
        report = self.client.costs("task 1", limit=1)
        self.assertEqual(report, EXECUTION["costs"])
        self.assertEqual(report["entries"][0]["delta_spent_microusd"], "-100")
        self.assertTrue(report["entries"][0]["synthetic"])
        self.assertTrue(report["financial_hold"])
        self.assertEqual(report["spent_microusd"], "9007199254740993")
        self.assertEqual(report["quarantines"][0]["actual_microusd"], "9223372036854775807")
        self.assertTrue(report["incomplete"])
        self.assertEqual(report["pending_quarantine_count"], 1)
        request = self.request()
        self.assertEqual(request["path"], "/api/v1/tasks/task%201/costs?limit=1")
        self.assertEqual(request["method"], "GET")
        self.assertIsNone(request["headers"].get("Idempotency-Key"))
        self.respond({"data": EXECUTION["costs"]})
        self.client.costs("task-1", limit=100,
            reservation_cursor=report["next_reservation_cursor"],
            entry_cursor=report["next_entry_cursor"],
            incident_cursor=report["next_incident_cursor"],
            quarantine_cursor=report["next_quarantine_cursor"])
        self.assertEqual(parse_qs(urlsplit(self.request()["path"]).query), {
            "limit": ["100"], "reservation_cursor": [report["next_reservation_cursor"]],
            "entry_cursor": [report["next_entry_cursor"]],
            "incident_cursor": [report["next_incident_cursor"]],
            "quarantine_cursor": [report["next_quarantine_cursor"]],
        })

    def test_available_models_are_paginated_authorized_reads(self) -> None:
        self.respond({"data": [EXECUTION["models"]], "next_cursor": "model:next+/=="})
        page = self.client.list_models(limit=1)
        self.assertEqual(page.data, [EXECUTION["models"]])
        first = self.request()
        self.assertEqual(first["path"], "/api/v1/models?limit=1")
        self.assertEqual(first["method"], "GET")
        self.assertIsNone(first["headers"].get("Idempotency-Key"))
        self.respond({"data": []})
        self.client.list_models(cursor=page.next_cursor)
        self.assertEqual(parse_qs(urlsplit(self.request()["path"]).query), {"cursor": [page.next_cursor]})

    def test_task_facts_require_exact_03_strings(self) -> None:
        for invalid in (0, None, "01", "-1", "9223372036854775808"):
            self.respond({"data": {"id": "task-1", "spent_microusd": invalid, "reserved_microusd": "0"}})
            with self.assertRaises(ProtocolError):
                self.client.get_task("task-1")
            self.request()
        self.respond({"data": [{"id": "task-1", "spent_microusd": 1000, "reserved_microusd": 0}]})
        with self.assertRaises(ProtocolError):
            self.client.list_tasks()
        self.request()
        self.respond({"data": {"id": "task-1", "spent_microusd": "9223372036854775807", "reserved_microusd": "0"}})
        self.assertEqual(self.client.get_task("task-1")["spent_microusd"], "9223372036854775807")
        self.request()

    def test_local_model_mode_and_exact_unknown_count(self) -> None:
        base = EXECUTION["models"]
        self.respond({"data": [{**base, "accounting_mode": "operator_unmetered_local", "max_charge_microusd": 0}]})
        self.assertEqual(self.client.list_models().data[0]["accounting_mode"], "operator_unmetered_local")
        self.request()
        for patch in ({"accounting_mode": "free"}, {"accounting_mode": "metered", "max_charge_microusd": 0},
                      {"accounting_mode": "operator_unmetered_local", "max_charge_microusd": 1}, {"timeout_milliseconds": 0}, {"max_input_tokens": True}):
            self.respond({"data": [{**base, **patch}]})
            with self.assertRaises(ProtocolError):
                self.client.list_models()
            self.request()
        self.respond({"data": {**EXECUTION["costs"], "unknown_microusd": "0", "unknown_reservation_count": "9007199254740993"}})
        self.assertEqual(self.client.costs("task-1")["unknown_reservation_count"], "9007199254740993")
        self.request()

    def test_nested_cost_money_rejects_numeric_and_noncanonical_values(self) -> None:
        for collection, field in (("reservations", "amount_microusd"), ("entries", "delta_spent_microusd"),
                                  ("incidents", "amount_microusd"), ("quarantines", "actual_microusd")):
            for invalid in (1000, "01", "-0", "0\n", "9223372036854775808"):
                with self.subTest(collection=collection, invalid=invalid):
                    import copy
                    report = copy.deepcopy(EXECUTION["costs"])
                    report[collection][0][field] = invalid
                    self.respond({"data": report})
                    with self.assertRaises(ProtocolError):
                        self.client.costs("task-1")
                    self.request()

    def test_costs_reject_invalid_report_shapes_and_summary_numbers(self) -> None:
        for patch in ({"reservations": None}, {"entries": [1]}, {"incidents": {}},
                      {"spent_microusd": 1.5}, {"unknown_microusd": -1},
                      {"unknown_reservation_count": 0}, {"unknown_reservation_count": False},
                      {"unknown_reservation_count": "01"}, {"unknown_reservation_count": "9223372036854775808"},
                      {"budget_microusd": True}, {"financial_hold": "false"},
                      {"next_entry_cursor": 1}, {"next_reservation_cursor": None},
                      {"quarantines": None}, {"pending_quarantine_count": -1},
                      {"pending_quarantine_count": True}, {"incomplete": "false"},
                      {"spent_microusd": "9223372036854775808"}, {"spent_microusd": "0\n"},
                      {"spent_microusd": "01"}, {"next_quarantine_cursor": 1}):
            with self.subTest(patch=patch):
                self.respond({"data": {**EXECUTION["costs"], **patch}})
                with self.assertRaises(ProtocolError):
                    self.client.costs("task-1")
                self.request()

    def test_new_reads_preserve_server_error_without_retry_or_fallback(self) -> None:
        for collection in ("runs", "operations", "actions", "approvals", "costs"):
            self.respond({"error": {"code": "not_found", "message": "Not found",
                                   "request_id": "req-hidden", "retryable": False}}, 404)
            with self.assertRaises(APIError) as raised:
                getattr(self.client, collection)("task-hidden")
            self.assertEqual(raised.exception.code, "not_found")
            self.assertEqual(self.request()["method"], "GET")

    def test_api_error_preserves_structured_metadata_without_automatic_retry(self) -> None:
        self.respond({"error": {
            "code": "worker_unavailable", "message": "Try again later", "request_id": "req-123",
            "retryable": True,
        }}, 503)
        with self.assertRaises(APIError) as raised:
            self.client.get_task("task-1")
        error = raised.exception
        self.assertEqual((error.status_code, error.code), (503, "worker_unavailable"))
        self.assertEqual(error.message, "Try again later")
        self.assertEqual(error.request_id, "req-123")
        self.assertTrue(error.retryable)
        self.request()

    def test_non_json_error_is_safe_and_preserves_request_header(self) -> None:
        self.respond(b"<html>private upstream diagnostics</html>", 502, **{"X-Request-ID": "req-proxy"})
        with self.assertRaises(APIError) as raised:
            self.client.list_tasks()
        self.assertEqual(raised.exception.request_id, "req-proxy")
        self.assertEqual(raised.exception.status_code, 502)
        self.assertNotIn("private upstream", str(raised.exception))
        self.request()

    def test_redirect_is_not_followed_with_bearer_credentials(self) -> None:
        self.respond({}, 302, Location=self.base_url + "/credential-leak")
        with self.assertRaises(APIError) as raised:
            self.client.get_task("task-1")
        self.assertEqual(raised.exception.status_code, 302)
        self.assertEqual(self.request()["path"], "/api/v1/tasks/task-1")

    def test_lost_response_raises_transport_error_without_resubmitting(self) -> None:
        self.respond(b"", 0)
        with self.assertRaises(TransportError):
            self.client.create_task("Goal", "agent-1", 100, idempotency_key="lost-response-1")
        self.assertEqual(self.request()["headers"]["Idempotency-Key"], "lost-response-1")

    def test_malformed_responses_raise_protocol_error(self) -> None:
        for response in (b"not json", [], {"items": []}, {"data": {}},
                         {"data": [1]}, {"data": [], "next_cursor": 7}):
            with self.subTest(response=response):
                self.respond(response)
                with self.assertRaises(ProtocolError):
                    self.client.list_tasks()
                self.request()

    def test_api_root_does_not_get_a_duplicate_suffix(self) -> None:
        client = Client(self.base_url + "/api/v1/", token="test", organization_id="org-1")
        self.respond({"data": []})
        client.list_tasks()
        self.assertEqual(self.request()["path"], "/api/v1/tasks")

    def test_invalid_requests_are_rejected_before_network_io(self) -> None:
        invalid = [
            lambda: self.client.create_task("Goal", "agent-1", True, idempotency_key="k"),
            lambda: self.client.create_task("Goal", "agent-1", -1, idempotency_key="k"),
            lambda: self.client.create_task("Goal", "agent-1", 0, idempotency_key="k"),
            lambda: self.client.create_task("Goal", "agent-1", 1_000_000_000_001, idempotency_key="k"),
            lambda: self.client.create_task("Goal", "agent-1", 1.5, idempotency_key="k"),
            lambda: self.client.create_task("Goal", "agent-1", 1, idempotency_key=""),
            lambda: self.client.create_task("Goal", "agent-1", 1, idempotency_key="x" * 129),
            lambda: self.client.create_task("Goal", "agent-1", 1, idempotency_key="key with spaces"),
            lambda: self.client.publish_agent("a", expected_version=True, idempotency_key="k"),
            lambda: self.client.events("t", after=8),
            lambda: self.client.list_tasks(limit=101),
            lambda: self.client.runs("t", limit=0),
            lambda: self.client.operations("t", limit=True),
            lambda: self.client.actions("t", limit=101),
            lambda: self.client.approvals("t", limit=1.5),
            lambda: self.client.costs("t", limit=101),
            lambda: self.client.costs("../secrets"),
            lambda: self.client.get_task("../secrets"),
            lambda: Client(self.base_url, token="test\r\nX-Other: value", organization_id="org-1"),
            lambda: Client("https://user:pass@example.com", token="test", organization_id="org-1"),
            lambda: Client(self.base_url, token="test", organization_id="org-1", timeout=0),
        ]
        for operation in invalid:
            with self.subTest(operation=operation):
                with self.assertRaises(ValueError):
                    operation()


if __name__ == "__main__":
    unittest.main()
