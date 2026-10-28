"""Shared contract and real bounded HTTP stream tests; no provider calls."""
import json
from pathlib import Path
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from test_client import ContractServer
from agent_platform import Client, ProtocolError, APIError
from agent_platform.events import EventStreamParser, event, batch, snapshot, _json

ROOT = Path(__file__).resolve().parents[2]
ORG, TASK, NOW = 'org_contract', 'task_contract', '2026-10-24T12:00:00Z'
LEGACY = dict(id='legacy', organization_id=ORG, task_id=TASK, sequence='9007199254740993', type='old.unknown', payload_version=0, legacy_data_json='{ "wide":9007199254740993, "note":"я" }', created_at=NOW)


def value(events=None, **extra):
    return dict(organization_id=ORG, task_id=TASK, events=[LEGACY] if events is None else events, cursor='opaque_cursor', has_more=False, reset_required=False, window=dict(after_sequence='0', through_sequence='9223372036854775807', version=1), **extra)


def frame(batch_value):
    return f'event: platform.events\r\nid: {batch_value["cursor"]}\r\ndata: {json.dumps(dict(frame_version=1,kind="batch",batch=batch_value),ensure_ascii=False)}\r\n\r\n'.encode()


class EventContractTests(unittest.TestCase):
    def test_shared_public_corpus_every_kind_variant_and_wide_legacy(self):
        corpus = json.loads((ROOT/'contracts/event-conformance.json').read_text())
        self.assertEqual(corpus['contract_version'], '0.14.0')
        for case in corpus['cases']:
            with self.subTest(name=case['name']):
                if case['valid']:
                    self.assertEqual(event(case['event'], ORG, TASK), case['event'])
                else:
                    with self.assertRaises(ProtocolError): event(case['event'], ORG, TASK)

    def test_arbitrary_byte_splits_newlines_and_heartbeat_preserve_exact_wide_text(self):
        wire = b': heartbeat\r\n\r\n' + frame(value()).replace(b'data: {', b'data: {\r\ndata: ')
        parser, received = EventStreamParser(ORG,TASK), []
        for char in wire: received.extend(parser.push(bytes([char])))
        parser.finish()
        self.assertEqual(received[0]['batch'], value())
        for delimiter in (b'\n', b'\r', b'\r\n'):
            parser = EventStreamParser(ORG,TASK)
            self.assertEqual(len(parser.push(frame(value()).replace(b'\r\n',delimiter))),1)
            parser.finish()

    def test_partial_oversized_utf8_duplicate_fields_and_unsafe_json_fail_closed(self):
        for wire in (frame(value())[:-2], frame(value()).replace(b'id: opaque_cursor', b'id: different'), frame(value()).replace(b'id: opaque_cursor', b'id: opaque_cursor\nid: opaque_cursor'), frame(value()).replace(b'data:', b'retry: 1\ndata:'), b'data: '+b' '*65536, b'\xff\n'):
            with self.subTest(wire_size=len(wire)), self.assertRaises(ProtocolError):
                parser=EventStreamParser(ORG,TASK);parser.push(wire);parser.finish()
        for text in ('{"kind":"batch","kind":"error"}', '{"a":9007199254740993}', '{"a":9007199254740991.1}', '{"a":1e99999}', '['*40+'0'+']'*40):
            with self.assertRaises(ProtocolError): _json(text)

    def test_scope_snapshot_and_explicit_history_reset(self):
        reset={**value([]),'cursor':'','reset_required':True,'reset_reason':'history_expired'}
        wire=('event: platform.reset\ndata: '+json.dumps(dict(frame_version=1,kind='reset_required',batch=reset))+'\n\n').encode()
        parser=EventStreamParser(ORG,TASK);self.assertEqual(parser.push(wire)[0]['kind'],'reset_required');parser.finish()
        with self.assertRaises(ProtocolError):EventStreamParser(ORG,TASK).push(wire.replace(b'data:',b'id: forbidden\ndata:'))
        for item in ({**value([]),'cursor':''},value([LEGACY,{**LEGACY,'id':'another'}]),value([{**LEGACY,'organization_id':'foreign'}])):
            with self.assertRaises(ProtocolError):batch(item,ORG,TASK)
        item=dict(organization_id=ORG,task_id=TASK,task=dict(id=TASK,organization_id=ORG,reserved_microusd='0',spent_microusd='0'),events=[LEGACY],cursor='head',replay_cursor='floor',window=value()['window'],has_earlier_events=False,observed_at=NOW)
        self.assertEqual(snapshot(item,ORG,TASK),item)
        with self.assertRaises(ProtocolError):snapshot({**item,'task':dict(id=TASK,organization_id='foreign')},ORG,TASK)

    def test_http_wire_version_and_snapshot_feed_no_mutations(self):
        server=ContractServer();thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start()
        client=Client(f'http://127.0.0.1:{server.server_port}',token='synthetic-private',organization_id=ORG)
        try:
            server.responses.put((200,{'data':value()},{}));self.assertEqual(client.task_event_feed(TASK,cursor='opaque_cursor',limit=1)['cursor'],'opaque_cursor');request=server.requests.get(timeout=1);self.assertEqual(request['headers']['X-Platform-API-Version'],'0.14.0');self.assertIsNone(request['body']);self.assertIn('cursor=opaque_cursor',request['path'])
            server.responses.put((200,{'data':[]},{'X-Platform-API-Version':'0.10.0'}))
            with self.assertRaises(ProtocolError):client.list_tasks()
            server.requests.get(timeout=1);self.assertTrue(server.requests.empty())
        finally:server.shutdown();server.server_close();thread.join(timeout=2)

    def test_real_http_one_connection_replay_auth_truncation_and_explicit_close(self):
        requests=[];mode=['stream'];closed=threading.Event()
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                requests.append(dict(path=self.path,headers=self.headers));self.send_response(401 if mode[0]=='denied' else 200);self.send_header('X-Platform-API-Version','0.14.0');self.send_header('Content-Type','application/json' if mode[0]=='denied' else 'text/event-stream');self.end_headers()
                if mode[0]=='denied':self.wfile.write(b'{"error":{"code":"unauthenticated","message":"Revoked","retryable":false}}');return
                self.wfile.write(frame(value()) if mode[0]!='partial' else frame(value())[:-2]);self.wfile.flush();closed.set()
            def log_message(self,*_args):pass
        server=ThreadingHTTPServer(('127.0.0.1',0),Handler);thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start();client=Client(f'http://127.0.0.1:{server.server_port}',token='synthetic-private',organization_id=ORG)
        try:
            for expected in (1,2):
                with client.stream_task_events(TASK,cursor='previous_cursor') as frames:
                    self.assertEqual(next(frames)['batch']['events'][0]['sequence'],LEGACY['sequence'])
                self.assertEqual(len(requests),expected)
            self.assertEqual(requests[0]['headers']['Authorization'],'Bearer synthetic-private');self.assertEqual(requests[0]['headers']['X-Platform-API-Version'],'0.14.0');self.assertNotIn('synthetic-private',requests[0]['path']);self.assertNotIn('Idempotency-Key',requests[0]['headers'])
            mode[0]='partial'
            with self.assertRaises(ProtocolError),client.stream_task_events(TASK) as frames:list(frames)
            mode[0]='denied'
            with self.assertRaises(APIError) as error:
                with client.stream_task_events(TASK) as frames:list(frames)
            self.assertEqual(error.exception.status_code,401);self.assertEqual(len(requests),4)
        finally:server.shutdown();server.server_close();thread.join(timeout=2)


    def test_window_revision_two_and_strict_legacy_timestamp_regressions(self):
        window=dict(after_sequence='9007199254740992',through_sequence='9223372036854775807',version=2)
        reset={**value([]),'cursor':'','reset_required':True,'reset_reason':'history_expired','window':window}
        self.assertEqual(batch(reset,ORG,TASK)['window']['version'],2)
        item=dict(organization_id=ORG,task_id=TASK,task=dict(id=TASK,organization_id=ORG,reserved_microusd='0',spent_microusd='0'),events=[LEGACY],cursor='newhead',replay_cursor='newfloor',window=window,has_earlier_events=True,observed_at=NOW)
        self.assertEqual(snapshot(item,ORG,TASK)['window']['version'],2)
        with self.assertRaises(ProtocolError):snapshot({**item,'task':{**item['task'],'reserved_microusd':1}},ORG,TASK)
        for patch in ({'legacy_data_json':''},{'created_at':'2026-10-24 12:00:00+00:00'},{'created_at':'2026-02-30T12:00:00Z'}):
            with self.assertRaises(ProtocolError):event({**LEGACY,**patch},ORG,TASK)
        for version in (0,1.5,2147483648,True):
            with self.assertRaises(ProtocolError):batch({**value([]),'window':{**window,'version':version}},ORG,TASK)

    def test_duplicate_actual_http_version_headers_are_ambiguous_even_if_identical(self):
        requests=[];versions=[['0.14.0','0.14.0'],['0.14.0','0.10.0']]
        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                requests.append(self.path);self.rfile.read(int(self.headers['Content-Length']));self.send_response(200)
                for version in versions[len(requests)-1]:self.send_header('X-Platform-API-Version',version)
                self.send_header('Content-Type','application/json');self.end_headers();self.wfile.write(b'{"data":{"id":"ambiguous"}}')
            def log_message(self,*_args):pass
        server=ThreadingHTTPServer(('127.0.0.1',0),Handler);thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start();client=Client(f'http://127.0.0.1:{server.server_port}',token='synthetic-private',organization_id=ORG)
        try:
            for count in (1,2):
                with self.assertRaises(ProtocolError):client.create_agent('Draft','Instructions',idempotency_key='exact-'+str(count))
                self.assertEqual(len(requests),count,'No hidden command resubmission')
        finally:server.shutdown();server.server_close();thread.join(timeout=2)
