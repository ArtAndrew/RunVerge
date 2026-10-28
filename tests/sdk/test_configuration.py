# SPDX-License-Identifier: Apache-2.0
"""Configuration public wire and HTTP contracts, using synthetic data only."""
import json
from pathlib import Path
import threading
import unittest
from urllib.parse import urlsplit, parse_qs
from test_client import ContractServer
from agent_platform import Client, APIError, ProtocolError
from agent_platform.configuration import configuration_facts, configuration_input

F = json.loads(Path(__file__).with_name('configuration-fixtures.json').read_text())


class ConfigurationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = ContractServer()
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown(); cls.server.server_close(); cls.thread.join()

    def setUp(self):
        self.client = Client(f'http://127.0.0.1:{self.server.server_port}', token='synthetic-only', organization_id='org')

    def tearDown(self):
        self.assertTrue(self.server.requests.empty()); self.assertTrue(self.server.responses.empty())

    def respond(self, data):
        self.server.responses.put((200, {'data': data}, {}))

    def request(self):
        return self.server.requests.get(timeout=1)

    def test_all_ten_reads_use_current_scope_and_preserve_wide_revisions_and_cursors(self):
        cases = [('configuration_catalog', (), {}, F['catalog'], 'catalog'), ('configuration_destinations', (), {'cursor': 'opaque+/=', 'limit': 1}, [F['destination']], 'destinations'), ('configuration_resources', (), {}, [F['resource']], 'resources'), ('configuration_grants', (), {}, [F['grant']], 'grants'), ('configuration_grant', ('grant',), {}, F['grant'], 'grants/grant'), ('configuration_plans', (), {}, [F['plan']], 'plans'), ('configuration_plan', ('plan',), {}, F['plan'], 'plans/plan'), ('configuration_verifications', (), {'plan_id': 'plan', 'cursor': 'opaque+/=', 'limit': 1}, [F['verification']], 'verifications'), ('configuration_verification', ('verification',), {}, F['verification'], 'verifications/verification'), ('configuration_audit', (), {}, [F['audit']], 'audit')]
        for method, args, kwargs, data, path in cases:
            self.respond(data); result = getattr(self.client, method)(*args, **kwargs)
            self.assertEqual(result.data if isinstance(data, list) else result, data)
            req = self.request(); parsed = urlsplit(req['path'])
            self.assertEqual(parsed.path, '/api/v1/configuration/' + path)
            self.assertIsNone(req['body']); self.assertNotIn('Idempotency-Key', req['headers'])
            if path == 'verifications': self.assertEqual(parse_qs(parsed.query)['plan_id'], ['plan'])
        self.assertEqual(F['catalog']['revision'], '9007199254740993')

    def test_seven_mutations_require_explicit_key_and_transmit_exact_wire_false(self):
        cases = [('create_configuration_grant', (), 'createGrant', 'grant', 'grants'), ('revoke_configuration_grant', ('grant',), 'revokeGrant', 'grant', 'grants/grant/revoke'), ('create_configuration_plan', (), 'createPlan', 'plan', 'plans'), ('validate_configuration_plan', ('plan',), 'validatePlan', 'validation', 'plans/plan/validate'), ('approve_configuration_plan', ('plan',), 'approvePlan', 'approval', 'plans/plan/approve'), ('apply_configuration_plan', ('plan',), 'applyPlan', 'plan', 'plans/plan/apply'), ('start_configuration_verification', ('plan',), 'verify', 'verification', 'plans/plan/verify')]
        for method, args, input_kind, output, path in cases:
            self.respond(F[output]); result = getattr(self.client, method)(*args, F['requests'][input_kind], idempotency_key='key-' + input_kind)
            self.assertEqual(result, F[output]); req = self.request()
            self.assertEqual(req['path'], '/api/v1/configuration/' + path)
            self.assertEqual(req['body'], F['requests'][input_kind]); self.assertEqual(req['headers']['Idempotency-Key'], 'key-' + input_kind)
        self.assertIs(F['requests']['createPlan']['operations'][0]['active'], False)
        with self.assertRaises(TypeError): self.client.create_configuration_plan(F['requests']['createPlan'])

    def test_invalid_input_rejects_before_network_without_coercion_or_private_fields(self):
        for patch in ({'base_revision': 9007199254740993}, {'base_revision': '01'}, {'base_revision': '1\n'}, {'base_revision': '9223372036854775808'}, {'ttl_seconds': 3601}, {'endpoint': 'private'}, {'operations': [dict(F['requests']['createPlan']['operations'][0], active=None)]}, {'operations': F['requests']['createPlan']['operations'] * 2}):
            with self.subTest(patch=patch), self.assertRaises(ValueError): self.client.create_configuration_plan(dict(F['requests']['createPlan'], **patch), idempotency_key='bad')
        for patch in ({'max_charge_microusd': 1000}, {'max_charge_microusd': '1000000000001'}, {'acknowledge_cost': False}, {'acknowledge_cost': 1}, {'credential_environment': 'PRIVATE'}, {'reason': '\ud800'}):
            with self.subTest(patch=patch), self.assertRaises(ValueError): self.client.start_configuration_verification('plan', dict(F['requests']['verify'], **patch), idempotency_key='bad')
        for patch in ({'parent_grant_id': 'parent'}, {'expected_parent_version': 1}, {'actions': ['inspect', 'inspect']}, {'destinations': F['requests']['createGrant']['destinations'] * 2}):
            with self.assertRaises(ValueError): configuration_input('createGrant', dict(F['requests']['createGrant'], **patch))

    def test_strict_response_scope_and_nested_private_field_boundaries(self):
        for patch in ({'organization_id': 'foreign'}, {'revision': 9007199254740993}, {'revision': '01'}, {'revision': '9223372036854775808'}, {'endpoint': 'private'}, {'metadata': {'token': 'private'}}):
            self.respond(dict(F['catalog'], **patch))
            with self.assertRaises(ProtocolError): self.client.configuration_catalog()
            self.request()
        for patch in ({'id': 'foreign'}, {'organization_id': 'foreign'}, {'operations': [dict(F['plan']['operations'][0], scope=dict(F['plan']['operations'][0]['scope'], credential_environment='PRIVATE'))]}, {'diff': [dict(F['plan']['diff'][0], trusted_ca_pem='private')]}, {'digest': 'bad'}, {'created_at': '2026-10-25 07:00:00+00:00'}):
            self.respond(dict(F['plan'], **patch))
            with self.assertRaises(ProtocolError): self.client.configuration_plan('plan')
            self.request()
        for patch in ({'plan_id': 'foreign'}, {'max_charge_microusd': 1000}, {'tool_count': True}, {'scope': dict(F['verification']['scope'], endpoint='private')}):
            self.respond([dict(F['verification'], **patch)])
            with self.assertRaises(ProtocolError): self.client.configuration_verifications(plan_id='plan')
            self.request()
        for record_type in ('catalog', 'destination', 'resource', 'grant', 'plan', 'validation', 'approval', 'verification', 'audit'):
            self.assertEqual(configuration_facts(record_type, F[record_type], 'org'), F[record_type])

    def test_unknown_cost_and_approval_obligation_do_not_claim_success_or_free(self):
        validation = configuration_facts('validation', F['validation'], 'org')
        self.assertTrue(validation['valid']); self.assertEqual(validation['decision']['obligations'], ['requires_approval'])
        verification = configuration_facts('verification', F['verification'], 'org')
        self.assertEqual((verification['status'], verification['cost_status']), ('unknown', 'unknown'))

    def test_permission_conflict_and_malformed_response_never_retry_POST(self):
        for status, code in ((403, 'forbidden'), (409, 'conflict'), (404, 'not_found')):
            self.server.responses.put((status, {'error': dict(code=code, message='Denied', retryable=False)}, {}))
            with self.assertRaises(APIError) as result: self.client.start_configuration_verification('plan', F['requests']['verify'], idempotency_key='stable')
            self.assertEqual(result.exception.code, code); self.assertEqual(self.request()['headers']['Idempotency-Key'], 'stable')
        self.server.responses.put((0, {}, {}))
        from agent_platform import TransportError
        with self.assertRaises(TransportError): self.client.start_configuration_verification('plan', F['requests']['verify'], idempotency_key='lost')
        self.request()
        self.server.responses.put((200, {'data': F['plan']}, {'X-Platform-API-Version': 'incompatible'}))
        with self.assertRaises(ProtocolError): self.client.apply_configuration_plan('plan', F['requests']['applyPlan'], idempotency_key='once')
        self.request()
        self.respond({'id': 'malformed'})
        with self.assertRaises(ProtocolError): self.client.start_configuration_verification('plan', F['requests']['verify'], idempotency_key='stable')
        self.request()
        self.assertFalse(hasattr(self.client, 'install_configuration_destination'))


if __name__ == '__main__': unittest.main()
