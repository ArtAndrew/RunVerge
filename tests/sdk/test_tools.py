"""Independent HTTP checks: exact intent, explicit dispatch, unknown never retried."""
import copy
import threading
import unittest
from urllib.parse import parse_qs, urlsplit
from test_client import ContractServer
from agent_platform import APIError, Client, ProtocolError

TASK={"id":"task","version":15,"reserved_microusd":"9007199254740993","spent_microusd":"0"}
VIEW={"task":TASK,"execution":{"id":"execution","version":2,"status":"awaiting_approval","max_charge_microusd":"1000000000000"},"tool":{"id":"tool","max_charge_microusd":"1000000000000"},"approval":{"id":"approval","version":1},"available_commands":["approve","deny"]}
PREPARE=dict(occurrence_id="intent.1",expected_task_version=7,candidate_id="candidate",artifact_id="artifact",artifact_version=2,content_digest="a"*64,tool_id="tool",tool_version=3,arguments_json=' {"large":9007199254740993,"decimal":1.0000000000000000001} ',expires_in_seconds=1)
CAS=dict(expected_task_version=15,expected_execution_version=2,expected_approval_version=1,intent_hash="b"*64)

class ToolHTTPTests(unittest.TestCase):
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
    def test_prepare_keeps_exact_json_lexemes_and_returned_task_version(self):
        self.respond({"data":VIEW},201)
        result=self.client.prepare_tool_execution("task",**PREPARE,idempotency_key="prepare")
        request=self.request();self.assertEqual(request["path"],"/api/v1/tasks/task/tool-executions");self.assertEqual(request["body"],PREPARE);self.assertEqual(request["headers"]["Idempotency-Key"],"prepare");self.assertEqual(result["task"]["version"],15);self.assertEqual(result["execution"]["max_charge_microusd"],"1000000000000")
    def test_approval_only_posts_decision_without_dispatch_or_cost_ack(self):
        self.respond({"data":VIEW});self.client.decide_tool_execution("task","execution",**CAS,decision="approve",reason="Checked exact intent",idempotency_key="decision")
        request=self.request();self.assertEqual(request["path"],"/api/v1/tasks/task/tool-executions/execution/decide");self.assertEqual(request["body"],{**CAS,"decision":"approve","reason":"Checked exact intent"})
    def test_dispatch_requires_explicit_true_and_exact_three_versions(self):
        for ack in [False,1,None]:
            with self.assertRaises(ValueError):self.client.dispatch_tool_execution("task","execution",**CAS,acknowledge_cost=ack,idempotency_key="dispatch")
        self.respond({"data":VIEW});self.client.dispatch_tool_execution("task","execution",**CAS,acknowledge_cost=True,idempotency_key="dispatch")
        request=self.request();self.assertTrue(request["path"].endswith("/dispatch"));self.assertEqual(request["body"],{**CAS,"acknowledge_cost":True})
    def test_reconcile_empty_exact_result_is_operator_assertion_no_accounting_mutation(self):
        self.respond({"data":VIEW});fields={k:v for k,v in CAS.items() if k!="expected_approval_version"};fields.update(outcome="rejected",effect_reference="external-ref",result="",evidence_digest="c"*64,reason="Operator observation")
        self.client.reconcile_tool_execution("task","execution",**fields,idempotency_key="reconcile");request=self.request();self.assertTrue(request["path"].endswith("/reconcile"));self.assertEqual(request["body"],fields)
    def test_list_metadata_and_exact_view_use_distinct_paginated_reads(self):
        for method,args,path,item in [("tools",(),"/tools",VIEW["tool"]),("tool_executions",("task",),"/tasks/task/tool-executions",VIEW["execution"])]:
            self.respond({"data":[item],"next_cursor":"next+/="});page=getattr(self.client,method)(*args,cursor="cursor+/=",limit=1);self.assertEqual(page.next_cursor,"next+/=");request=self.request();url=urlsplit(request["path"]);self.assertEqual(url.path,"/api/v1"+path);self.assertEqual(parse_qs(url.query),{"cursor":["cursor+/="],"limit":["1"]});self.assertIsNone(request["headers"].get("Idempotency-Key"))
        self.respond({"data":VIEW});self.assertEqual(self.client.get_tool_execution("task","execution"),VIEW);self.assertEqual(self.request()["path"],"/api/v1/tasks/task/tool-executions/execution")
    def test_invalid_bounds_duplicate_keys_and_object_shape_fail_before_http(self):
        for patch in [{"arguments_json":v} for v in ['[]','null','"text"','{"n":NaN}','{"a":1,"\\u0061":2}','{"nested":{"a":1,"a":2}}','{"a":"'+('я'*16385)+'"}','{"a":"\ud800"}']]+[{"occurrence_id":"bad\n"},{"expires_in_seconds":0},{"expires_in_seconds":86401},{"expected_task_version":True},{"content_digest":"a"*64+"\n"}]:
            with self.subTest(patch=tuple(patch)),self.assertRaises((ValueError,UnicodeError)):self.client.prepare_tool_execution("task",**{**PREPARE,**patch},idempotency_key="bad")
        for decision in ["approved","denied"]:
            with self.assertRaises(ValueError):self.client.decide_tool_execution("task","execution",**CAS,decision=decision,reason="Reason",idempotency_key="bad")
    def test_unknown_conflict_is_exposed_without_retry(self):
        self.respond({"error":{"code":"conflict","message":"Outcome unknown","request_id":"r","retryable":False}},409)
        with self.assertRaises(APIError) as raised:self.client.dispatch_tool_execution("task","execution",**CAS,acknowledge_cost=True,idempotency_key="dispatch")
        self.assertEqual(raised.exception.code,"conflict");self.assertEqual(self.request()["headers"]["Idempotency-Key"],"dispatch")
    def test_unsafe_response_financial_numbers_are_not_coerced(self):
        for value in [9007199254740993,"1000000000001","01","-1","9223372036854775808"]:
            fixture=copy.deepcopy(VIEW);fixture["execution"]["max_charge_microusd"]=value;self.respond({"data":fixture})
            with self.subTest(value=value),self.assertRaises(ProtocolError):self.client.get_tool_execution("task","execution")
            self.request()
    def test_missing_nested_view_components_are_protocol_errors(self):
        for field in ("task", "execution", "approval", "tool"):
            self.respond({"data":{**VIEW,field:None}})
            with self.subTest(field=field),self.assertRaises(ProtocolError):self.client.get_tool_execution("task","execution")
            self.request()
