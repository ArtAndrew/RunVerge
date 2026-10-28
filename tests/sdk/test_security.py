"""Security metadata contracts; synthetic HTTP, never credential issuance."""
import threading
import unittest
from urllib.parse import parse_qs,urlsplit
from test_client import ContractServer
from agent_platform import Client,APIError,ProtocolError
NOW='2026-10-24T10:00:00Z'
POLICY=dict(organization_id='org',mode='mixed',oidc_issuer='',service_access_enabled=True,max_service_key_ttl_seconds=3600,version=2,created_at=NOW,updated_at=NOW,available_commands=['update'],can_require_oidc=False,readiness_reasons=['oidc_not_configured','browser_owner_required'])
ACCOUNT=dict(organization_id='org',principal_id='service',principal_kind='service',display_name='Integration',role='member',active=True,principal_active=True,version=2,created_at=NOW,updated_at=NOW,available_commands=['update'],assignable_roles=['member','viewer'])
KEY=dict(id='key',organization_id='org',principal_id='service',role_ceiling='viewer',version=1,revoked=False,created_at=NOW,expires_at='2026-10-24T11:00:00Z',status='active',available_commands=['revoke'])
AUDIT=dict(id='audit',organization_id='org',actor_type='trusted_host',object_type='key',object_id='key',target_principal_id='service',action='service_key.issued',reason='Integration setup',after=dict(version=1,role_ceiling='viewer',expires_at=KEY['expires_at'],revoked=False),created_at=NOW)
class SecurityHTTPTests(unittest.TestCase):
 @classmethod
 def setUpClass(cls):cls.server=ContractServer();cls.thread=threading.Thread(target=cls.server.serve_forever,daemon=True);cls.thread.start()
 @classmethod
 def tearDownClass(cls):cls.server.shutdown();cls.server.server_close();cls.thread.join()
 def setUp(self):self.client=Client(f'http://127.0.0.1:{self.server.server_port}',token='test-only',organization_id='org')
 def tearDown(self):self.assertTrue(self.server.requests.empty());self.assertTrue(self.server.responses.empty())
 def respond(self,value,status=200):self.server.responses.put((status,value,{}))
 def request(self):return self.server.requests.get(timeout=1)
 def test_reads_all_metadata_routes_preserve_scope_and_opaque_cursor(self):
  for call,data,path in ((self.client.get_login_policy,POLICY,'/login-policy'),(lambda:self.client.list_service_accounts(cursor='opaque+/=',limit=1),[ACCOUNT],'/service-accounts'),(lambda:self.client.get_service_account('service'),ACCOUNT,'/service-accounts/service'),(lambda:self.client.list_service_keys('service',cursor='next+/=',limit=1),[KEY],'/service-accounts/service/keys'),(lambda:self.client.get_service_key('service','key'),KEY,'/service-accounts/service/keys/key'),(lambda:self.client.list_security_audit(principal_id='service',cursor='audit+/=',limit=1),[AUDIT],'/security-audit')):
   self.respond({'data':data});value=call();self.assertEqual(value.data if isinstance(data,list) else value,data);req=self.request();self.assertEqual(urlsplit(req['path']).path,'/api/v1'+path);self.assertIsNone(req['body']);self.assertNotIn('Idempotency-Key',req['headers'])
   if path=='/security-audit':self.assertEqual(parse_qs(urlsplit(req['path']).query),{'principal_id':['service'],'cursor':['audit+/='],'limit':['1']})
 def test_all_mutations_send_exact_CAS_required_false_and_explicit_key(self):
  self.respond({'data':POLICY});self.client.update_login_policy(expected_version=2,mode='mixed',oidc_issuer='',service_access_enabled=False,max_service_key_ttl_seconds=60,reason='Disable applications',idempotency_key='policy');r=self.request();self.assertEqual(r['path'],'/api/v1/login-policy/update');self.assertEqual(r['body'],dict(expected_version=2,mode='mixed',oidc_issuer='',service_access_enabled=False,max_service_key_ttl_seconds=60,reason='Disable applications'));self.assertEqual(r['headers']['Idempotency-Key'],'policy')
  self.respond({'data':ACCOUNT},201);self.client.create_service_account('Integration','member',reason='Fresh identity',idempotency_key='create');self.assertEqual(self.request()['body'],dict(display_name='Integration',role='member',reason='Fresh identity'))
  self.respond({'data':{**ACCOUNT,'active':False}});self.client.update_service_account('service',expected_version=2,display_name='Integration',role='viewer',active=False,reason='Disable',idempotency_key='update');r=self.request();self.assertEqual(r['path'],'/api/v1/service-accounts/service/update');self.assertIs(r['body']['active'],False)
  self.respond({'data':{**KEY,'revoked':True,'version':2}});self.client.revoke_service_key('service','key',expected_version=1,reason='Rotate key',idempotency_key='revoke');r=self.request();self.assertEqual(r['path'],'/api/v1/service-accounts/service/keys/key/revoke');self.assertEqual(r['body'],dict(expected_version=1,reason='Rotate key'))
 def test_invalid_boolean_roles_bounds_text_and_identity_reject_before_network(self):
  base=dict(expected_version=2,mode='mixed',oidc_issuer='',service_access_enabled=True,max_service_key_ttl_seconds=3600,reason='Review',idempotency_key='bad')
  for patch in ({'expected_version':True},{'service_access_enabled':None},{'service_access_enabled':0},{'max_service_key_ttl_seconds':0},{'max_service_key_ttl_seconds':86401},{'mode':'open'},{'reason':' '},{'oidc_issuer':'\ud800'}):
   with self.subTest(patch=patch),self.assertRaises((ValueError,TypeError)):self.client.update_login_policy(**{**base,**patch})
  for role in ('owner','admin','root'):
   with self.assertRaises(ValueError):self.client.create_service_account('Integration',role,reason='Review',idempotency_key='bad')
  for active in (None,0,'false'):
   with self.assertRaises(ValueError):self.client.update_service_account('service',expected_version=2,display_name='Integration',role='viewer',active=active,reason='Review',idempotency_key='bad')
  for value in ('','../other','a/b'):
   with self.assertRaises(ValueError):self.client.get_service_key('service',value)
  self.assertFalse(hasattr(self.client,'issue_service_token'))
 def test_invalid_scope_target_kind_and_secret_responses_are_rejected(self):
  for patch in ({'organization_id':'other'},{'principal_id':'other'},{'principal_kind':'human'},{'role':'owner'},{'active':None},{'version':True},{'token':'must-not-return'},{'raw_token':'must-not-return'},{'api_key':'must-not-return'},{'metadata':{'nested':[{'token':'must-not-return'}]}}):
   self.respond({'data':{**ACCOUNT,**patch}})
   with self.assertRaises(ProtocolError):self.client.get_service_account('service')
   self.request()
  for patch in ({'id':'other'},{'principal_id':'other'},{'role_ceiling':'admin'},{'token_hash':'must-not-return'},{'expires_at':'invalid'}):
   self.respond({'data':{**KEY,**patch}})
   with self.assertRaises(ProtocolError):self.client.get_service_key('service','key')
   self.request()
  for patch in ({'organization_id':'other'},{'version':1.5},{'service_access_enabled':0},{'readiness_reasons':'ready'}):
   self.respond({'data':{**POLICY,**patch}})
   with self.assertRaises(ProtocolError):self.client.get_login_policy()
   self.request()
 def test_filtered_audit_must_match_target_and_nested_state_cannot_leak_secret(self):
  for patch in ({'target_principal_id':'other'},{'after':{**AUDIT['after'],'token':'must-not-return'}},{'after':{'version':True}}):
   self.respond({'data':[{**AUDIT,**patch}]})
   with self.assertRaises(ProtocolError):self.client.list_security_audit(principal_id='service')
   self.request()
 def test_fresh_identity_requires_exact_org_explicit_kind_and_no_nested_credentials(self):
  me=dict(organization_id='org',principal_id='owner',principal_kind='human',role='owner',permissions=['identity.manage'])
  for patch in ({'organization_id':'other'},{'principal_kind':None},{'principal_kind':'unknown'},{'permissions':'identity.manage'},{'metadata':{'api_key':'must-not-return'}}):
   self.respond({'data':{**me,**patch}})
   with self.assertRaises(ProtocolError):self.client.get_current_identity()
   self.request()
  missing=dict(me);missing.pop('principal_kind');self.respond({'data':missing})
  with self.assertRaises(ProtocolError):self.client.get_current_identity()
  self.request()
  for value in (dict(POLICY,metadata=[0]*10001),dict(POLICY,metadata={'safe':'ordinary extension'})):
   self.respond({'data':value})
   if isinstance(value['metadata'],list):
    with self.assertRaises(ProtocolError):self.client.get_login_policy()
   else:self.assertEqual(self.client.get_login_policy()['metadata'],value['metadata'])
   self.request()
 def test_current_permission_and_CAS_errors_never_automatically_retry(self):
  for code,status in (('forbidden',403),('conflict',409),('invalid_transition',409)):
   self.respond({'error':{'code':code,'message':'Policy rejected','retryable':False}},status)
   with self.assertRaises(APIError) as result:self.client.revoke_service_key('service','key',expected_version=1,reason='Review',idempotency_key='stable')
   self.assertEqual(result.exception.code,code);self.assertEqual(self.request()['headers']['Idempotency-Key'],'stable')
if __name__=='__main__':unittest.main()
