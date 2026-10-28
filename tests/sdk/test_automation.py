"""Explicit automation delegation HTTP boundaries; no automatic execution."""
import threading
import unittest
from urllib.parse import parse_qs, urlsplit
from test_client import ContractServer
from agent_platform import APIError, Client, ProtocolError, AutomationToolPin, TaskAutomationState

PINS = [{"tool_id":"read","tool_version":2,"tool_profile_digest":"a"*64}]
CREATE = dict(expected_task_version=7,mode="read_only",tool_pins=PINS,max_model_turns=4,max_tool_executions=8,
              max_charge_microusd="1234567",expires_in_seconds=900,acknowledge_cost=True,reason="Task-scoped delegated read")
STATE = {"usage":{"model_turns":2,"tool_executions":3,"allocated_microusd":"9007199254740993"},"signals":[],"available_commands":["create"]}
GRANTED = {**STATE,"grant":{"id":"grant","version":1,"max_charge_microusd":"1234567"},"available_commands":["revoke"]}

class AutomationHTTPTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server=ContractServer();cls.thread=threading.Thread(target=cls.server.serve_forever,daemon=True);cls.thread.start()
    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown();cls.server.server_close();cls.thread.join()
    def setUp(self):self.client=Client(f"http://127.0.0.1:{self.server.server_port}",token="test-only",organization_id="org")
    def tearDown(self):self.assertTrue(self.server.requests.empty());self.assertTrue(self.server.responses.empty())
    def respond(self,data,status=200):self.server.responses.put((status,data,{}))
    def request(self):return self.server.requests.get(timeout=1)
    def test_read_preserves_exact_cumulative_usage_without_mutation(self):
        self.respond({"data":STATE});self.assertEqual(self.client.automation("task"),STATE)
        request=self.request();self.assertEqual(request["path"],"/api/v1/tasks/task/automation");self.assertIsNone(request["body"]);self.assertNotIn("Idempotency-Key",request["headers"])
    def test_create_sends_exact_pins_string_money_and_cumulative_limits(self):
        self.respond({"data":GRANTED},201);self.assertEqual(self.client.create_automation("task",**CREATE,idempotency_key="grant"),GRANTED)
        request=self.request();self.assertEqual(request["path"],"/api/v1/tasks/task/automation");self.assertEqual(request["headers"]["Idempotency-Key"],"grant");self.assertEqual(request["body"],CREATE)
    def test_prepare_mode_and_explicit_revoke_have_separate_commands(self):
        self.respond({"data":GRANTED},201);self.client.create_automation("task",**{**CREATE,"mode":"prepare_and_continue"},idempotency_key="prepare-mode")
        self.assertEqual(self.request()["body"]["mode"],"prepare_and_continue")
        self.respond({"data":STATE});self.client.revoke_automation("task",grant_id="grant",expected_version=2,reason="Stop autonomous work",idempotency_key="revoke")
        request=self.request();self.assertEqual(request["path"],"/api/v1/tasks/task/automation/revoke");self.assertEqual(request["body"],{"grant_id":"grant","expected_version":2,"reason":"Stop autonomous work"})
    def test_invalid_limits_money_pins_and_implicit_ack_reject_before_http(self):
        patches=[{"acknowledge_cost":False},{"acknowledge_cost":1},{"mode":"all_actions"},{"tool_pins":[]},{"tool_pins":PINS*17},{"tool_pins":PINS*2},
                 {"tool_pins":[{**PINS[0],"tool_version":True}]},{"tool_pins":[{**PINS[0],"tool_profile_digest":"bad"}]},{"expected_task_version":0},
                 {"max_model_turns":32},{"max_tool_executions":33},{"expires_in_seconds":86401},{"max_charge_microusd":"0"},{"max_charge_microusd":"01"},
                 {"max_charge_microusd":1},{"max_charge_microusd":"1000000000001"},{"max_charge_microusd":"1e3"},{"reason":" "},{"reason":"\ud800"},{"reason":"я"*2049}]
        for patch in patches:
            with self.subTest(patch=tuple(patch)),self.assertRaises((ValueError,TypeError)):self.client.create_automation("task",**{**CREATE,**patch},idempotency_key="bad")
        for patch in [{"grant_id":""},{"expected_version":True},{"reason":" "}]:
            with self.subTest(patch=tuple(patch)),self.assertRaises(ValueError):self.client.revoke_automation("task",**{"grant_id":"grant","expected_version":1,"reason":"Stop",**patch},idempotency_key="bad")
    def test_progress_signals_paginate_without_rewriting_evidence(self):
        signal={"id":"signal","code":"repeated_intent","evidence_ids":["old-run:proposal","new-run:proposal"]}
        self.respond({"data":[signal],"next_cursor":"next+/="});page=self.client.progress_signals("task",cursor="prior+/=",limit=1)
        self.assertEqual(page.data,[signal]);self.assertEqual(page.next_cursor,"next+/=");url=urlsplit(self.request()["path"])
        self.assertEqual(url.path,"/api/v1/tasks/task/progress-signals");self.assertEqual(parse_qs(url.query),{"cursor":["prior+/="],"limit":["1"]})
    def test_stale_grant_never_retries_and_malformed_response_money_rejects(self):
        self.respond({"error":{"code":"conflict","message":"Task changed","retryable":False}},409)
        with self.assertRaises(APIError) as error:self.client.create_automation("task",**CREATE,idempotency_key="stable")
        self.assertEqual(error.exception.code,"conflict");self.assertEqual(self.request()["headers"]["Idempotency-Key"],"stable")
        for value in [{**STATE,"usage":{**STATE["usage"],"allocated_microusd":9007199254740992}}, {**STATE,"usage":{**STATE["usage"],"model_turns":True}},
                      {**GRANTED,"grant":{**GRANTED["grant"],"max_charge_microusd":"01"}}, {**STATE,"available_commands":"create"}]:
            self.respond({"data":value})
            with self.assertRaises(ProtocolError):self.client.automation("task")
            self.request()
