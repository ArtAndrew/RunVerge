"""HTTP contract checks for exact model proposals and separately charged continuation."""
import threading
import unittest
from urllib.parse import parse_qs, urlsplit
from test_client import ContractServer
from test_tools import PREPARE, VIEW, TASK
from agent_platform import APIError, Client, ModelToolProposal, ModelToolResultBinding

RESULTS = [{"proposal_id":"proposal-one","execution_id":"exec-one","execution_version":5,"status":"succeeded","receipt_id":"receipt-one","content_digest":"c"*64},
           {"proposal_id":"proposal-two","execution_id":"exec-two","execution_version":3,"status":"denied"}]
CONTINUE = dict(expected_task_version=15,candidate_id="candidate",artifact_id="artifact",artifact_version=1,content_digest="a"*64,
                results=RESULTS,acknowledge_cost=True,reason="Use these exact completed observations")

class ModelLoopHTTPTests(unittest.TestCase):
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
    def test_metadata_and_exact_proposal_read_keep_argument_lexemes(self):
        self.respond({"data":[{"id":"proposal"}],"next_cursor":"opaque+/="})
        page=self.client.model_proposals("task",cursor="older+/=",limit=1)
        self.assertEqual(page.next_cursor,"opaque+/=");url=urlsplit(self.request()["path"])
        self.assertEqual(url.path,"/api/v1/tasks/task/model-tool-proposals");self.assertEqual(parse_qs(url.query),{"cursor":["older+/="],"limit":["1"]})
        exact={"id":"proposal","arguments_json":' {"n":9007199254740993} '}
        self.respond({"data":exact});self.assertEqual(self.client.get_model_proposal("task","proposal"),exact)
        self.assertEqual(self.request()["path"],"/api/v1/tasks/task/model-tool-proposals/proposal")
    def test_prepare_binds_proposal_without_rewriting_exact_arguments(self):
        self.respond({"data":VIEW},201)
        fields={**PREPARE,"occurrence_id":"model:proposal","model_proposal_id":"proposal"}
        self.client.prepare_tool_execution("task",**fields,idempotency_key="native-prepare")
        request=self.request();self.assertEqual(request["body"],fields);self.assertEqual(request["headers"]["Idempotency-Key"],"native-prepare")
    def test_continue_sends_all_exact_terminal_bindings_and_ack(self):
        self.respond({"data":TASK});result=self.client.continue_model("task",**CONTINUE,idempotency_key="continue")
        request=self.request();self.assertEqual(request["path"],"/api/v1/tasks/task/continue-model");self.assertEqual(request["body"],CONTINUE)
        self.assertEqual(request["headers"]["Idempotency-Key"],"continue");self.assertEqual(result["reserved_microusd"],"9007199254740993")
    def test_unknown_partial_duplicate_or_receipt_substitution_rejected_before_http(self):
        patches=[{"acknowledge_cost":False},{"acknowledge_cost":1},{"results":[]},{"results":RESULTS*5},{"results":[RESULTS[0],RESULTS[0]]},
                 {"results":[{**RESULTS[0],"status":"unknown"}]},{"results":[{**RESULTS[0],"receipt_id":""}]},
                 {"results":[{**RESULTS[0],"content_digest":"bad"}]},{"results":[{**RESULTS[1],"receipt_id":"forged"}]},
                 {"results":[{**RESULTS[0],"execution_version":True}]},{"reason":" "}]
        for patch in patches:
            with self.subTest(fields=tuple(patch)),self.assertRaises(ValueError):self.client.continue_model("task",**{**CONTINUE,**patch},idempotency_key="bad")
    def test_stale_continuation_is_not_replayed_or_turned_into_new_attempt(self):
        self.respond({"error":{"code":"conflict","message":"Exact receipt changed","retryable":False}},409)
        with self.assertRaises(APIError) as error:self.client.continue_model("task",**CONTINUE,idempotency_key="same-intent")
        self.assertEqual(error.exception.code,"conflict");self.assertEqual(self.request()["headers"]["Idempotency-Key"],"same-intent")
