// SPDX-License-Identifier: Elastic-2.0
import type {CurrentIdentity,OrganizationLoginPolicy,ServiceAccount,ServiceKey,OrganizationSecurityAuditEntry,ServiceRole,LoginPolicyMode} from '../../packages/typescript-sdk/src/types'
import {memberText} from './members.ts'
export type {OrganizationLoginPolicy,ServiceAccount,ServiceKey,OrganizationSecurityAuditEntry,ServiceRole,LoginPolicyMode}
export interface UpdateLoginPolicy {expected_version:number;mode:LoginPolicyMode;oidc_issuer:string;service_access_enabled:boolean;max_service_key_ttl_seconds:number;reason:string}
export interface CreateServiceAccount {display_name:string;role:ServiceRole;reason:string}
export interface UpdateServiceAccount extends CreateServiceAccount {expected_version:number;active:boolean}
export interface RevokeServiceKey {expected_version:number;reason:string}
const record=(v:unknown):v is Record<string,unknown>=>Boolean(v&&typeof v==='object'&&!Array.isArray(v));
const strings=(v:unknown):v is string[]=>Array.isArray(v)&&v.every(x=>typeof x==='string');
const positive=(v:unknown)=>Number.isSafeInteger(v)&&Number(v)>0;
const role=(v:unknown)=>v==='member'||v==='viewer';
const mode=(v:unknown)=>v==='mixed'||v==='oidc_required';
function securityText(v:unknown,max:number,empty=false):v is string{return typeof v==='string'&&(empty||Boolean(v.trim()))&&!v.includes('\0')&&!/[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/u.test(v)&&new TextEncoder().encode(v).length<=max;}
const date=(v:unknown)=>securityText(v,128)&&Number.isFinite(Date.parse(v));
// Reject credential-shaped fields even in unknown extensions. Work is bounded;
// opaque user text is not classified as a secret by this protocol check.
const credentialNames=new Set(['token','rawtoken','accesstoken','refreshtoken','idtoken','sessiontoken','bearertoken','secret','clientsecret','password','privatekey','apikey','authorization','credential','credentials','tokenhash','tokenprefix','secretreference','credentialhash','tokendigest']);
function noSecret(value:Record<string,unknown>){const pending:[unknown,number][]=[[value,0]];let nodes=0;while(pending.length){const [v,depth]=pending.pop()!;if(++nodes>10000||depth>32)return false;if(!v||typeof v!=='object')continue;if(Array.isArray(v)){if(nodes+pending.length+v.length>10000)return false;for(const x of v)pending.push([x,depth+1]);continue}for(const k in v){if(!Object.hasOwn(v,k))continue;if(nodes+pending.length+1>10000||credentialNames.has(k.replace(/[^a-z0-9]/gi,'').toLowerCase()))return false;pending.push([(v as Record<string,unknown>)[k],depth+1])}}return true;}
export const credentialFree=(v:unknown)=>record(v)&&noSecret(v);
function facts(v:unknown,org:string):asserts v is Record<string,unknown>{if(!record(v)||v.organization_id!==org||!positive(v.version)||!noSecret(v))throw new Error('Invalid security response');}
function loginPolicyFacts(value:unknown,org:string):OrganizationLoginPolicy{
 facts(value,org);if(!mode(value.mode)||!securityText(value.oidc_issuer,2048,true)||typeof value.service_access_enabled!=='boolean'||!Number.isSafeInteger(value.max_service_key_ttl_seconds)||Number(value.max_service_key_ttl_seconds)<1||Number(value.max_service_key_ttl_seconds)>86400||!date(value.created_at)||!date(value.updated_at)||!strings(value.available_commands)||typeof value.can_require_oidc!=='boolean'||!strings(value.readiness_reasons)||(value.configured_oidc_issuer!==undefined&&!securityText(value.configured_oidc_issuer,2048)))throw new Error('Invalid login policy response');
 return value as unknown as OrganizationLoginPolicy;
}
function serviceAccountFacts(value:unknown,org:string,pid?:string):ServiceAccount{
 facts(value,org);if(value.principal_kind!=='service'||!securityText(value.principal_id,128)||(pid!==undefined&&value.principal_id!==pid)||!securityText(value.display_name,120)||!role(value.role)||typeof value.active!=='boolean'||typeof value.principal_active!=='boolean'||!date(value.created_at)||!date(value.updated_at)||!strings(value.available_commands)||!Array.isArray(value.assignable_roles)||!value.assignable_roles.every(role)||(value.credentials_revoked_at!==undefined&&!date(value.credentials_revoked_at)))throw new Error('Invalid service account response');
 return value as unknown as ServiceAccount;
}
function serviceKeyFacts(value:unknown,org:string,pid:string,key?:string):ServiceKey{
 facts(value,org);if(value.principal_id!==pid||!securityText(value.id,128)||(key!==undefined&&value.id!==key)||!role(value.role_ceiling)||typeof value.revoked!=='boolean'||!date(value.created_at)||!date(value.expires_at)||!securityText(value.status,128)||!strings(value.available_commands))throw new Error('Invalid service key response');
 return value as unknown as ServiceKey;
}
function auditState(v:unknown):boolean{return record(v)&&positive(v.version)&&noSecret(v)&&(v.mode===undefined||mode(v.mode))&&(v.oidc_issuer===undefined||securityText(v.oidc_issuer,2048,true))&&(v.service_access_enabled===undefined||typeof v.service_access_enabled==='boolean')&&(v.max_service_key_ttl_seconds===undefined||(Number.isSafeInteger(v.max_service_key_ttl_seconds)&&Number(v.max_service_key_ttl_seconds)>=1&&Number(v.max_service_key_ttl_seconds)<=86400))&&(v.display_name===undefined||securityText(v.display_name,120))&&(v.role===undefined||role(v.role))&&(v.active===undefined||typeof v.active==='boolean')&&(v.role_ceiling===undefined||role(v.role_ceiling))&&(v.expires_at===undefined||date(v.expires_at))&&(v.revoked===undefined||typeof v.revoked==='boolean');}
function securityAuditFacts(value:unknown,org:string,pid?:string):OrganizationSecurityAuditEntry{
 if(!record(value)||value.organization_id!==org||!noSecret(value)||!['id','actor_type','object_type','object_id','action'].every(k=>securityText(value[k],128))||!securityText(value.reason,4096)||!date(value.created_at)||!auditState(value.after)||(value.before!==undefined&&!auditState(value.before))||(value.actor_id!==undefined&&!securityText(value.actor_id,128))||(value.target_principal_id!==undefined&&!securityText(value.target_principal_id,128))||(pid!==undefined&&value.target_principal_id!==pid))throw new Error('Invalid security audit response');
 return value as unknown as OrganizationSecurityAuditEntry;
}

export const serviceRoles:Record<ServiceRole,string>={member:'Участник',viewer:'Просмотр'}
export const readinessLabels:Record<string,string>={oidc_not_configured:'SSO ещё не настроен и не проверен сервером.',browser_owner_required:'Для включения SSO войдите через настроенного провайдера как человек-владелец.',issuer_mismatch:'Текущая сессия не соответствует доверенному провайдеру.'}
export const canAdminSecurity=(identity:CurrentIdentity|null)=>Boolean(identity&&identity.principal_kind==='human'&&['owner','admin'].includes(identity.role)&&identity.permissions.includes('identity.manage'))
export function validLoginPolicy(v:unknown,org:string):v is OrganizationLoginPolicy{try{loginPolicyFacts(v,org);return true}catch{return false}}
export function validServiceAccount(v:unknown,org:string,pid?:string):v is ServiceAccount{try{serviceAccountFacts(v,org,pid);return true}catch{return false}}
export function validServiceKey(v:unknown,org:string,pid:string,key?:string):v is ServiceKey{try{serviceKeyFacts(v,org,pid,key);return true}catch{return false}}
export function validSecurityAudit(v:unknown,org:string,pid?:string):v is OrganizationSecurityAuditEntry{try{securityAuditFacts(v,org,pid);return true}catch{return false}}
export function policyChanged(p:OrganizationLoginPolicy,v:Omit<UpdateLoginPolicy,'reason'|'expected_version'>){return p.mode!==v.mode||p.oidc_issuer!==v.oidc_issuer||p.service_access_enabled!==v.service_access_enabled||p.max_service_key_ttl_seconds!==v.max_service_key_ttl_seconds}
export function policyPayload(p:OrganizationLoginPolicy,mode:LoginPolicyMode,issuer:string,enabled:boolean,ttl:number,reason:string):UpdateLoginPolicy{
 if(!p.available_commands.includes('update')||!['mixed','oidc_required'].includes(mode)||typeof enabled!=='boolean'||!Number.isSafeInteger(ttl)||ttl<1||ttl>86400||!memberText(reason,4096))throw new Error('Проверьте право изменения, срок ключей и причину.')
 const requiring=p.mode!==mode||issuer!==p.oidc_issuer
 if(requiring&&(!p.can_require_oidc||!p.configured_oidc_issuer||(mode==='oidc_required'&&issuer!==p.configured_oidc_issuer)))throw new Error('Сервер ещё не подтвердил готовность и вход владельца через доверенный SSO.')
 if(typeof issuer!=='string'||issuer.includes('\0')||new TextEncoder().encode(issuer).length>2048)throw new Error('Некорректный провайдер входа.')
 const value={mode,oidc_issuer:issuer,service_access_enabled:enabled,max_service_key_ttl_seconds:ttl}
 if(!policyChanged(p,value))throw new Error('Измените настройку перед сохранением.')
 return{expected_version:p.version,...value,reason}
}
export function servicePayload(name:string,role:ServiceRole,reason:string,allowed:string[],account?:ServiceAccount,active?:boolean):CreateServiceAccount|UpdateServiceAccount{
 if(!memberText(name,120)||!memberText(reason,4096)||!['member','viewer'].includes(role)||!allowed.includes(role))throw new Error('Проверьте имя, разрешённую роль и причину.')
 if(!account)return{display_name:name,role,reason}
 if(typeof active!=='boolean'||!account.available_commands.includes('update'))throw new Error('Подтвердите состояние и право изменения.')
 if(account.display_name===name&&account.role===role&&account.active===active)throw new Error('Измените имя, роль или состояние перед сохранением.')
 return{expected_version:account.version,display_name:name,role,active,reason}
}
export function serviceIssueCommand(org:string,principal:string,ttl=3600){if(!/^[A-Za-z0-9_-]{1,128}$/.test(org)||!/^[A-Za-z0-9_-]{1,128}$/.test(principal))return null;return`platform issue-service-token --organization ${org} --principal ${principal} --role-ceiling viewer --ttl ${Math.max(1,Math.min(3600,Number.isSafeInteger(ttl)?ttl:3600))}s --reason 'Подключение интеграции' --out /secure/new-service-key.json`}
