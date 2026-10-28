"""Independent HTTP checks for exact-candidate review and explicit new attempts."""
import json
import threading
import unittest
from urllib.parse import parse_qs, urlsplit
from test_client import ContractServer
from agent_platform import APIError, Client

TASK = {"id":"task-1","status":"blocked","reserved_microusd":"150","spent_microusd":"0","attempt_count":1,"max_runs":3,"version":9,"execution_deadline":"2026-10-24T00:00:00Z"}
REVIEW = dict(expected_version=8,candidate_id="candidate-1",artifact_id="artifact-1",artifact_version=1,content_digest="a"*64,decision="request_changes",reason="Уточните вывод",idempotency_key="review-1")

class ReviewHTTPTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server=ContractServer();cls.thread=threading.Thread(target=cls.server.serve_forever,daemon=True);cls.thread.start()
    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown();cls.server.server_close();cls.thread.join()
    def setUp(self):
        self.client=Client(f"http://127.0.0.1:{self.server.server_port}",token="test-only",organization_id="org-1")
    def tearDown(self):
        self.assertTrue(self.server.requests.empty());self.assertTrue(self.server.responses.empty())
    def respond(self,data,status=200): self.server.responses.put((status,data,{}))
    def test_review_binds_exact_candidate_and_preserves_unknown_hold(self):
        self.respond({"data":TASK})
        task=self.client.review_task("task-1",**REVIEW)
        self.assertEqual(task["reserved_microusd"],"150")
        req=self.server.requests.get(timeout=1)
        self.assertEqual(req["path"],"/api/v1/tasks/task-1/review")
        self.assertEqual(req["headers"]["Idempotency-Key"],"review-1")
        self.assertEqual(req["body"],{k:v for k,v in REVIEW.items() if k!="idempotency_key"})
    def test_new_attempt_requires_explicit_ack_and_pins_feedback(self):
        args=dict(expected_version=9,reason="Revise the answer",feedback="Use the attached evidence",idempotency_key="new-attempt-1")
        for value in (False,None,1,"true"):
            with self.subTest(value=value),self.assertRaises(ValueError): self.client.retry_task("task-1",acknowledge_new_cost=value,**args)
        with self.assertRaises(TypeError): self.client.retry_task("task-1",**args)
        self.respond({"data":{**TASK,"version":10,"attempt_count":2,"status":"queued"}})
        self.client.retry_task("task-1",acknowledge_new_cost=True,**args)
        req=self.server.requests.get(timeout=1)
        self.assertEqual(req["path"],"/api/v1/tasks/task-1/retry")
        self.assertEqual(req["body"],{"expected_version":9,"reason":args["reason"],"feedback":args["feedback"],"acknowledge_new_cost":True})
    def test_decision_digest_reason_utf8_and_version_rejection_without_http(self):
        for replacement in ({"expected_version":0},{"artifact_version":True},{"content_digest":"a"*64+"\n"},{"content_digest":"A"*64},{"decision":"approve"},{"reason":" "},{"reason":"я"*2049},{"reason":"a\x00b"}):
            with self.subTest(replacement=replacement),self.assertRaises(ValueError): self.client.review_task("task-1",**{**REVIEW,**replacement})
        with self.assertRaises(ValueError): self.client.retry_task("task-1",expected_version=1,reason="Retry",feedback="я"*4097,acknowledge_new_cost=True,idempotency_key="retry")
    def test_candidates_decisions_children_have_independent_opaque_cursor(self):
        for name in ("candidates","decisions","children"):
            self.respond({"data":[TASK] if name=="children" else [{"id":name}],"next_cursor":"next+/="})
            result=getattr(self.client,name)("task-1",cursor="cursor+/=",limit=1)
            self.assertEqual(result.next_cursor,"next+/=")
            req=self.server.requests.get(timeout=1);url=urlsplit(req["path"])
            self.assertEqual(url.path,f"/api/v1/tasks/task-1/{name}");self.assertEqual(parse_qs(url.query),{"cursor":["cursor+/="],"limit":["1"]})
            self.assertIsNone(req["headers"].get("Idempotency-Key"))
    def test_conflict_is_not_retried_with_fresh_version(self):
        self.respond({"error":{"code":"conflict","message":"Changed","request_id":"r1","retryable":False}},409)
        with self.assertRaises(APIError) as raised: self.client.review_task("task-1",**REVIEW)
        self.assertEqual(raised.exception.code,"conflict");self.server.requests.get(timeout=1)
    def test_bounded_creation_and_team_member_version_are_explicit(self):
        self.respond({"data":TASK},201)
        self.client.create_task("Goal","agent",1000,max_runs=3,execution_timeout_seconds=3600,idempotency_key="task")
        req=self.server.requests.get(timeout=1);self.assertEqual(req["body"]["max_runs"],3);self.assertEqual(req["body"]["execution_timeout_seconds"],3600)
        team={"pattern":"parallel","members":[{"id":"a","agent_id":"agent-1","agent_version":2,"task":"Prepare evidence"}],"join_policy":"all","max_depth":1,"max_children":1,"max_parallel":1}
        self.respond({"data":{"id":"team","team":team}},201)
        self.client.create_agent("Team","Work together",model="team:coordinator",team=team,idempotency_key="team")
        self.assertEqual(self.server.requests.get(timeout=1)["body"]["team"],team)
        with self.assertRaises(ValueError): self.client.create_agent("Team","Work together",team={**team,"max_parallel":2},idempotency_key="team-bad")
