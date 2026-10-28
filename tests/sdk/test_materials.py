"""Independent HTTP checks for draft versions and exact candidate publication."""
import threading
import unittest
from urllib.parse import parse_qs, urlsplit
from test_client import ContractServer
from agent_platform import APIError, Client

HEAD={"id":"material-1","head_version":2,"head_content_digest":"a"*64}
TASK={"id":"task","reserved_microusd":"150","spent_microusd":"2000","candidate_id":"published","current_run_id":"same-run","attempt_count":1}

class MaterialHTTPTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server=ContractServer();cls.thread=threading.Thread(target=cls.server.serve_forever,daemon=True);cls.thread.start()
    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown();cls.server.server_close();cls.thread.join()
    def setUp(self): self.client=Client(f"http://127.0.0.1:{self.server.server_port}",token="test-only",organization_id="org")
    def tearDown(self): self.assertTrue(self.server.requests.empty());self.assertTrue(self.server.responses.empty())
    def respond(self,data,status=200): self.server.responses.put((status,data,{}))
    def test_current_identity_is_read_only_and_contains_no_credential(self):
        value={"principal_id":"principal","principal_kind":"human","organization_id":"org","role":"member","permissions":["task:read"]}
        self.respond({"data":value});self.assertEqual(self.client.get_current_identity(),value)
        req=self.server.requests.get(timeout=1);self.assertEqual(req["path"],"/api/v1/me");self.assertIsNone(req["headers"].get("Idempotency-Key"));self.assertIsNone(req["body"])
    def test_empty_draft_is_explicit_and_not_execution(self):
        self.respond({"data":HEAD},201);self.client.create_material("task","Notes","text/plain","",base_candidate_id="original",reason="Draft",idempotency_key="create")
        req=self.server.requests.get(timeout=1);self.assertEqual(req["path"],"/api/v1/tasks/task/materials");self.assertEqual(req["headers"]["Idempotency-Key"],"create");self.assertEqual(req["body"],{"name":"Notes","media_type":"text/plain","content":"","base_candidate_id":"original","reason":"Draft"})
    def test_revision_preserves_exact_text_and_manual_merge_base(self):
        self.respond({"data":HEAD});self.client.revise_material("task","material-1","  Текст\r\n",expected_version=4,merge_base_version=2,reason="Manual merge",idempotency_key="revision")
        req=self.server.requests.get(timeout=1);self.assertEqual(req["path"],"/api/v1/tasks/task/materials/material-1/revisions");self.assertEqual(req["body"],{"expected_version":4,"merge_base_version":2,"reason":"Manual merge","content":"  Текст\r\n"})
    def test_publish_binds_saved_head_and_parent_without_paid_retry(self):
        self.respond({"data":TASK});result=self.client.publish_material("task","material-1",expected_task_version=8,expected_material_version=2,content_digest="a"*64,parent_candidate_id="original",idempotency_key="publish")
        req=self.server.requests.get(timeout=1);self.assertEqual(req["path"],"/api/v1/tasks/task/materials/material-1/publish");self.assertEqual(req["body"],{"expected_task_version":8,"expected_material_version":2,"content_digest":"a"*64,"parent_candidate_id":"original"});self.assertEqual(result,TASK)
    def test_reads_have_opaque_pagination_and_explicit_exact_versions(self):
        for method,args,suffix in [("materials",(),"materials"),("material_revisions",("material-1",),"materials/material-1/revisions"),("artifacts",(),"artifacts")]:
            self.respond({"data":[HEAD],"next_cursor":"next+/="});page=getattr(self.client,method)("task",*args,cursor="cursor+/=",limit=1);self.assertEqual(page.next_cursor,"next+/=");req=self.server.requests.get(timeout=1);url=urlsplit(req["path"]);self.assertEqual(url.path,"/api/v1/tasks/task/"+suffix);self.assertEqual(parse_qs(url.query),{"cursor":["cursor+/="],"limit":["1"]});self.assertIsNone(req["headers"].get("Idempotency-Key"))
        for fn,path in [(lambda:self.client.get_material("task","material-1"),"/api/v1/tasks/task/materials/material-1"),(lambda:self.client.get_material_version("task","material-1",2),"/api/v1/tasks/task/materials/material-1/revisions/2"),(lambda:self.client.get_artifact("task","artifact",version=1),"/api/v1/tasks/task/artifacts/artifact?version=1")]:
            self.respond({"data":{"revision":{"version":2},"content":""}});fn();self.assertEqual(self.server.requests.get(timeout=1)["path"],path)
    def test_conflict_is_exposed_without_auto_merge_or_retry(self):
        self.respond({"error":{"code":"conflict","message":"Changed","request_id":"r1","retryable":False}},409)
        with self.assertRaises(APIError) as raised:self.client.revise_material("task","material-1","My exact buffer",expected_version=1,reason="Edit",idempotency_key="revision")
        self.assertEqual(raised.exception.code,"conflict");self.assertEqual(self.server.requests.get(timeout=1)["body"]["content"],"My exact buffer")
    def test_material_bounds_fail_before_http(self):
        for fields in [{"content":"я"*131073},{"content":"a\0b"},{"content":"\ud800"},{"media_type":"text/html"},{"name":"я"*61},{"name":" "},{"reason":"я"*2049},{"base_candidate_id":"x"*129}]:
            args={"name":"Notes","media_type":"text/plain","content":"",**fields}
            with self.subTest(fields=tuple(fields)),self.assertRaises((ValueError,UnicodeError)):self.client.create_material("task",**args,idempotency_key="bad")
        for fields in [{"expected_version":0},{"expected_version":129},{"expected_version":True},{"merge_base_version":3},{"merge_base_version":0},{"reason":" "}]:
            args={"expected_version":2,"reason":"Edit",**fields}
            with self.subTest(fields=fields),self.assertRaises(ValueError):self.client.revise_material("task","material","",**args,idempotency_key="bad")
        with self.assertRaises(ValueError):self.client.publish_material("task","material",expected_task_version=1,expected_material_version=1,content_digest="a"*64+"\n",parent_candidate_id="parent",idempotency_key="bad")
