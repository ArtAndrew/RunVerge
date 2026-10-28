# SPDX-License-Identifier: Apache-2.0
"""Synthetic product catalogue HTTP and hostile wire tests; no commands run."""
import copy
import json
from pathlib import Path
import threading
import unittest
from unittest.mock import patch
from test_client import ContractServer
from agent_platform import APIError, Client, ProductCapabilities, ProtocolError
from agent_platform.capabilities import capability_facts

F = json.loads(Path(__file__).with_name("capabilities-fixtures.json").read_text())


def changed(case):
    value = copy.deepcopy(F["normal"])
    for change in [case] + case.get("extra", []):
        parent = value
        for key in change["path"][:-1]:
            parent = parent[key]
        parent[change["path"][-1]] = copy.deepcopy(change["value"]) if "value" in change else [copy.deepcopy(F["normal"]["operations"][0])] * change["repeat"]
    return value


class CapabilityTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = ContractServer()
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown(); cls.server.server_close(); cls.thread.join()

    def setUp(self):
        self.client = Client(f"http://127.0.0.1:{self.server.server_port}", token="synthetic-private", organization_id="org")

    def tearDown(self):
        self.assertTrue(self.server.requests.empty()); self.assertTrue(self.server.responses.empty())

    def respond(self, value, status=200):
        self.server.responses.put((status, {"data": value}, {}))

    def test_read_uses_current_scope_without_query_body_or_mutation(self):
        self.respond(F["normal"])
        with patch("subprocess.run", side_effect=AssertionError("catalogue executed a command")), patch("os.system", side_effect=AssertionError("catalogue executed shell")):
            value: ProductCapabilities = self.client.capabilities()
        self.assertEqual(value, F["normal"])
        request = self.server.requests.get(timeout=1)
        self.assertEqual((request["method"], request["path"], request["body"]), ("GET", "/api/v1/capabilities", None))
        self.assertEqual(request["headers"]["Authorization"], "Bearer synthetic-private")
        self.assertEqual(request["headers"]["X-Organization-ID"], "org")
        self.assertEqual(request["headers"]["X-Platform-API-Version"], "0.14.0")
        self.assertNotIn("Idempotency-Key", request["headers"])
        self.assertNotIn("can_execute", value)

    def test_shared_hostile_wire_corpus_and_inert_display_text(self):
        for case in F["cases"]:
            with self.subTest(name=case["name"]):
                value = changed(case)
                if case["valid"]:
                    self.assertIs(capability_facts(value, "org"), value)
                else:
                    with self.assertRaises(ProtocolError) as error:
                        capability_facts(value, "org")
                    self.assertEqual(str(error.exception), "Invalid product capabilities response")
                    self.assertNotIn("private-response-value", str(error.exception))

    def test_generated_full_catalogue_accepts_all_six_roles_and_real_sdk_bindings(self):
        actual = json.loads((Path(__file__).resolve().parents[2] / "tests/contracts/fixtures/product-capabilities.json").read_text())
        self.assertIs(capability_facts(actual, "org_catalog_fixture", "catalog_fixture"), actual)
        catalog = json.loads((Path(__file__).resolve().parents[2] / "contracts/capability-catalog.json").read_text())
        for role in catalog["roles"]:
            value = dict(catalog, catalog_version="1", assessment="support_and_role_observation", read_only=True, observed_at="2026-10-26T10:00:00Z", identity=dict(role, organization_id="org", principal_id="actor"))
            with self.subTest(kind=role["principal_kind"], role=role["role"]):
                self.assertIs(capability_facts(value, "org"), value)
        for operation in catalog["operations"]:
            binding = next(b for b in operation["bindings"] if b["surface"] == "python")
            if binding["status"] == "available":
                self.assertTrue(callable(getattr(Client, binding["name"], None)), operation["id"])
            else:
                self.assertEqual(binding["name"], "")

    def test_missing_fields_wrong_types_unicode_and_bounded_traversal(self):
        for field in F["normal"]:
            value = copy.deepcopy(F["normal"]); del value[field]
            with self.subTest(field=field), self.assertRaises(ProtocolError):
                capability_facts(value, "org")
        for value in (None, [], {}, dict(F["normal"], profiles=[None]), dict(F["normal"], observed_at="\ud800"), dict(F["normal"], future={"nested": [0] * 50001})):
            with self.assertRaises(ProtocolError): capability_facts(value, "org")

    def test_largest_operation_count_and_guard_lists_fit_the_bounded_catalogue(self):
        value = copy.deepcopy(F["normal"])
        value["operations"] = []
        for index in range(128):
            operation = copy.deepcopy(F["normal"]["operations"][0])
            operation.update(id="operation_" + str(index), path="/api/v1/route_" + str(index), runtime_guards=["Current authorization and resource checks"] * 32)
            value["operations"].append(operation)
        value["operations"][0]["id"] = "get_capabilities"
        self.assertIs(capability_facts(value, "org"), value)

    def test_known_current_principal_cannot_switch_inside_catalogue(self):
        self.respond(F["normal"]["identity"])
        self.client.get_current_identity(); self.server.requests.get(timeout=1)
        foreign = copy.deepcopy(F["normal"]); foreign["identity"]["principal_id"] = "another"
        self.respond(foreign)
        with self.assertRaises(ProtocolError): self.client.capabilities()
        self.server.requests.get(timeout=1)

    def test_errors_do_not_follow_redirects_or_retry(self):
        for status in (401, 403, 409):
            self.server.responses.put((status, {"error": {"code": "forbidden", "message": "Denied", "retryable": False}}, {}))
            with self.assertRaises(APIError): self.client.capabilities()
            self.server.requests.get(timeout=1)
        self.server.responses.put((302, {}, {"Location": "http://127.0.0.1:1/private"}))
        with self.assertRaises(APIError) as error: self.client.capabilities()
        self.assertEqual(error.exception.status_code, 302)
        self.server.requests.get(timeout=1)
        self.assertFalse(any(type(handler).__name__ == "ProxyHandler" and handler.proxies for handler in self.client._opener.handlers))


if __name__ == "__main__": unittest.main()
