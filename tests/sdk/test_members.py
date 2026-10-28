"""Membership HTTP contracts; no real credentials, organization or identity lookup."""
import threading
import unittest
from urllib.parse import parse_qs,urlsplit
from test_client import ContractServer
from agent_platform import APIError,Client,ProtocolError

MEMBER=dict(organization_id='org',principal_id='principal',display_name='Локальное имя',role='member',active=True,principal_active=True,version=2,created_at='2026-10-24T10:00:00Z',updated_at='2026-10-24T10:00:00Z',available_commands=['update','revoke_credentials'],assignable_roles=['admin','member','viewer'])
CAPS=dict(organization_id='org',can_create_members=True,assignable_roles=['admin','member','viewer'],self_management_allowed=False)
AUDIT=dict(id='audit',organization_id='org',actor_id='admin',target_principal_id='principal',action='member.updated',reason='Access review',before=dict(display_name='Old',role='member',active=True,version=1),after={k:MEMBER[k] for k in ('display_name','role','active','version')},created_at=MEMBER['created_at'])
class MembershipHTTPTests(unittest.TestCase):
 @classmethod
 def setUpClass(cls):cls.server=ContractServer();cls.thread=threading.Thread(target=cls.server.serve_forever,daemon=True);cls.thread.start()
 @classmethod
 def tearDownClass(cls):cls.server.shutdown();cls.server.server_close();cls.thread.join()
 def setUp(self):self.client=Client(f'http://127.0.0.1:{self.server.server_port}',token='test-only',organization_id='org')
 def tearDown(self):self.assertTrue(self.server.requests.empty());self.assertTrue(self.server.responses.empty())
 def respond(self,data,status=200):self.server.responses.put((status,data,{}))
 def request(self):return self.server.requests.get(timeout=1)
 def test_capabilities_list_exact_get_and_audit_are_read_only_and_scoped(self):
  for caps in (CAPS,{**CAPS,'can_create_members':False,'assignable_roles':[]}):
   self.respond({'data':caps});self.assertEqual(self.client.membership_capabilities(),caps);self.assertEqual(self.request()['path'],'/api/v1/members/capabilities')
  self.respond({'data':[MEMBER],'next_cursor':'opaque+/='});page=self.client.list_members(cursor='previous+/=',limit=1);self.assertEqual(page.data,[MEMBER]);self.assertEqual(page.next_cursor,'opaque+/=');request=self.request();self.assertEqual(parse_qs(urlsplit(request['path']).query),{'cursor':['previous+/='],'limit':['1']});self.assertIsNone(request['body']);self.assertNotIn('Idempotency-Key',request['headers'])
  self.respond({'data':MEMBER});self.assertEqual(self.client.get_member('principal'),MEMBER);self.assertEqual(self.request()['path'],'/api/v1/members/principal')
  self.respond({'data':[AUDIT]});self.assertEqual(self.client.list_membership_audit(principal_id='principal',cursor='next+/=',limit=1).data,[AUDIT]);request=self.request();self.assertEqual(urlsplit(request['path']).path,'/api/v1/membership-audit');self.assertEqual(parse_qs(urlsplit(request['path']).query),{'principal_id':['principal'],'cursor':['next+/='],'limit':['1']})
 def test_create_has_no_claimed_identity_secret_or_email_and_preserves_local_name(self):
  self.respond({'data':MEMBER},201);self.assertEqual(self.client.create_member('Локальное имя','member',reason='Provision only',idempotency_key='create'),MEMBER);request=self.request();self.assertEqual(request['body'],{'display_name':'Локальное имя','role':'member','reason':'Provision only'});self.assertEqual(request['headers']['Idempotency-Key'],'create')
 def test_update_sends_required_false_and_exact_version_while_revoke_is_separate(self):
  self.respond({'data':{**MEMBER,'active':False}});self.client.update_member('principal',expected_version=2,display_name='Local',role='viewer',active=False,reason='Disable access',idempotency_key='update');request=self.request();self.assertEqual(request['path'],'/api/v1/members/principal/update');self.assertEqual(request['body'],{'expected_version':2,'display_name':'Local','role':'viewer','active':False,'reason':'Disable access'})
  self.respond({'data':MEMBER});self.client.revoke_member_credentials('principal',expected_version=2,reason='Invalidate old org logins',idempotency_key='revoke');request=self.request();self.assertEqual(request['path'],'/api/v1/members/principal/revoke-credentials');self.assertEqual(request['body'],{'expected_version':2,'reason':'Invalidate old org logins'})
 def test_invalid_inputs_and_missing_status_reject_without_http(self):
  for patch in ({'active':None},{'active':0},{'active':'false'},{'expected_version':True},{'expected_version':0},{'display_name':' '},{'display_name':'я'*61},{'display_name':'\ud800'},{'role':'superadmin'},{'reason':' '},{'reason':'a'*4097}):
   with self.subTest(fields=tuple(patch)),self.assertRaises((TypeError,ValueError)):
    self.client.update_member('principal',**{**dict(expected_version=2,display_name='Local',role='member',active=True,reason='Access review'),**patch},idempotency_key='invalid')
  with self.assertRaises(TypeError):self.client.update_member('principal',expected_version=2,display_name='Local',role='member',reason='No status',idempotency_key='missing')
  for value in (0,101,True):
   with self.assertRaises(ValueError):self.client.list_members(limit=value)
  for value in ('','../other','a/b'):
   with self.assertRaises(ValueError):self.client.revoke_member_credentials(value,expected_version=2,reason='Review',idempotency_key='invalid')
 def test_conflicts_self_policy_and_permission_loss_never_autoretry(self):
  for code,status in (('conflict',409),('invalid_transition',409),('forbidden',403)):
   self.respond({'error':{'code':code,'message':'Rejected current policy','retryable':False}},status)
   with self.assertRaises(APIError) as result:self.client.revoke_member_credentials('principal',expected_version=2,reason='Review',idempotency_key='stable')
   self.assertEqual(result.exception.code,code);self.assertEqual(self.request()['headers']['Idempotency-Key'],'stable')
 def test_malformed_cross_org_and_wrong_target_results_never_report_success(self):
  for value in ({**MEMBER,'organization_id':'foreign'},{**MEMBER,'principal_id':'other'},{**MEMBER,'active':None},{**MEMBER,'version':True},{**MEMBER,'available_commands':'update'},{**MEMBER,'assignable_roles':['root']},{'id':'not-member'}):
   self.respond({'data':value})
   with self.assertRaises(ProtocolError):self.client.get_member('principal')
   self.request()
  self.respond({'data':{**CAPS,'self_management_allowed':True}})
  with self.assertRaises(ProtocolError):self.client.membership_capabilities()
  self.request()
  self.respond({'data':[{**AUDIT,'organization_id':'foreign'}]})
  with self.assertRaises(ProtocolError):self.client.list_membership_audit()
  self.request()
if __name__=='__main__':unittest.main()
