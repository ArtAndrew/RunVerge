// SPDX-License-Identifier: Apache-2.0
// Bounded, strict public configuration protocol. No host configuration is accepted.
import {ProtocolError} from './errors.ts';
import type * as T from './configurationTypes.js';
type Check=(v:unknown)=>boolean;
type Fields=Record<string,Check>;
const rec=(v:unknown):v is Record<string,unknown>=>Boolean(v&&typeof v==='object'&&!Array.isArray(v));
const text=(n:number,empty=false):Check=>v=>typeof v==='string'&&(empty||Boolean(v.trim()))&&!v.includes('\0')&&!/[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/u.test(v)&&new TextEncoder().encode(v).length<=n;
const int=(lo=0,hi=2147483647):Check=>v=>Number.isSafeInteger(v)&&Number(v)>=lo&&Number(v)<=hi;
const bool:Check=v=>typeof v==='boolean';
const one=(...values:unknown[]):Check=>v=>values.includes(v);
const list=(c:Check,min=0,max=100):Check=>v=>Array.isArray(v)&&v.length>=min&&v.length<=max&&v.every(c);
const decimal=(max='9223372036854775807'):Check=>v=>typeof v==='string'&&/^(0|[1-9][0-9]*)(?![\s\S])/.test(v)&&v.length<=max.length&&(v.length<max.length||v<=max);
const date:Check=v=>typeof v==='string'&&v.length<=64&&/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?(?:Z|[+-]\d\d:\d\d)(?![\s\S])/.test(v)&&Number.isFinite(Date.parse(v));
const sha:Check=v=>typeof v==='string'&&/^[a-f0-9]{64}(?![\s\S])/.test(v);
const id=text(128),reason=text(4096),kind=one('model','mcp'),audience=one('platform.configuration.v1'),actions=list(one('inspect','plan','validate','apply','verify','delegate'),0,6),commands=list(text(128),0,32),money=decimal('1000000000000');
function shape(required:Fields,optional:Fields={}):Check{return v=>rec(v)&&Object.keys(v).length<=Object.keys(required).length+Object.keys(optional).length&&Object.entries(required).every(([k,c])=>Object.hasOwn(v,k)&&c(v[k]))&&Object.entries(v).every(([k,value])=>Object.hasOwn(required,k)||Object.hasOwn(optional,k)&&optional[k]!(value));}
const scope=shape({destination_id:id,destination_version:int(1),kind,target_id:id,credential_purpose_id:id,credential_purpose_version:int(1)});
const diff=shape({operation_id:id,kind,target_id:id,expected_version:int(),new_version:int(1),active:bool,retired_versions:list(int(1),0,10000),may_stop_pinned_tasks:bool});
const operation=shape({id,scope,expected_resource_version:int(),active:bool});
const destination=shape({id,organization_id:id,version:int(1),kind,name:text(120),target_id:id,credential_purpose_id:id,credential_purpose_version:int(1),requires_approval:bool,max_verify_charge_microusd:money,discovery_unmetered_approved:bool,active:bool,created_at:date},{model:text(256)});
const catalog=shape({organization_id:id,revision:decimal(),audience,actions,can_issue_root_grants:bool,can_approve:bool,max_operations:int(1,8),max_grant_depth:int(1,4),available_commands:commands});
const grant=shape({id,organization_id:id,audience,issuer_id:id,subject_id:id,depth:int(0,4),version:int(1),actions,scopes:list(scope,1,16),max_plans:int(1,100),used_plans:int(0,100),max_verifications:int(1,100),used_verifications:int(0,100),max_allocated_charge_microusd:money,allocated_charge_microusd:money,revoked:bool,status:text(128),created_at:date,expires_at:date,available_commands:commands},{parent_grant_id:id});
const plan=shape({id,organization_id:id,actor_id:id,version:int(1),status:text(128),base_revision:decimal(),digest:sha,operations:list(operation,1,8),diff:list(diff,1,8),reason,created_at:date,expires_at:date,applied_revision:decimal(),available_commands:commands},{grant_id:id,applied_at:date,receipt_id:id});
const decision=shape({id,organization_id:id,action:text(128),target_id:id,outcome:text(128),reason_codes:list(text(128),0,100),configuration_revision:decimal(),obligations:list(text(128),0,100),evaluated_at:date},{grant_id:id});
const validation=shape({id,organization_id:id,plan_id:id,plan_version:int(1),plan_digest:sha,base_revision:decimal(),current_revision:decimal(),valid:bool,decision,diff:list(diff,1,8),created_at:date});
const approval=shape({id,organization_id:id,plan_id:id,plan_digest:sha,validation_id:id,principal_id:id,reason,created_at:date});
const verification=shape({id,organization_id:id,plan_id:id,operation_id:id,actor_id:id,scope,resource_version:int(1),kind,status:text(128),version:int(1),max_charge_microusd:money,cost_status:text(128),tool_count:int(),created_at:date,updated_at:date},{grant_id:id,task_id:id,protocol_version:text(128),catalog_digest:sha,response_digest:sha,reason_code:text(128)});
const resource=shape({organization_id:id,kind,id,version:int(1),scope,active:bool,created_at:date});
const audit=shape({id,organization_id:id,actor_type:text(128),object_type:text(128),object_id:id,action:text(128),digest:text(64,true),reason:text(4096,true),created_at:date},{actor_id:id});
const checks={catalog,destination,grant,plan,validation,approval,verification,resource,audit};
interface Records {catalog:T.ConfigurationCatalog;destination:T.ConfigurationDestination;grant:T.CapabilityEnvelope;plan:T.ConfigurationPlan;validation:T.ConfigurationValidation;approval:T.ConfigurationApproval;verification:T.ConfigurationVerification;resource:T.ConfigurationResource;audit:T.ConfigurationAuditEntry}
/** Reject unknown fields, including nested private profiles; bind every record to its requested organization/target. */
export function configurationFacts<K extends keyof Records>(type:K,value:unknown,org:string,target?:string,planId?:string):Records[K]{
 if(!Object.hasOwn(checks,type)||!checks[type](value)||!rec(value)||value.organization_id!==org||(target!==undefined&&value.id!==target)||(planId!==undefined&&value.plan_id!==planId))throw new ProtocolError('Invalid public configuration response');
 if(type==='validation'&&(!rec(value.decision)||value.decision.organization_id!==org||value.decision.target_id!==value.plan_id))throw new ProtocolError('Invalid configuration decision scope');
 return value as unknown as Records[K];
}
const versionDigest={expected_version:int(1),plan_digest:sha};
const opInput=shape({id,destination_id:id,destination_version:int(1),expected_resource_version:int(),active:bool});
const requestChecks={
 createGrant:shape({subject_id:id,actions:list(one('inspect','plan','validate','apply','verify','delegate'),1,6),destinations:list(shape({id,version:int(1)}),1,16),ttl_seconds:int(1,86400),max_plans:int(1,100),max_verifications:int(1,100),max_allocated_charge_microusd:money,reason},{parent_grant_id:id,expected_parent_version:int(1)}),
 revokeGrant:shape({expected_version:int(1),reason}),
 createPlan:shape({base_revision:decimal(),ttl_seconds:int(1,3600),operations:list(opInput,1,8),reason},{grant_id:id}),
 validatePlan:shape(versionDigest),approvePlan:shape({...versionDigest,validation_id:id,reason}),applyPlan:shape({...versionDigest,validation_id:id,reason}),
 verify:shape({expected_plan_version:int(1),plan_digest:sha,operation_id:id,max_charge_microusd:money,acknowledge_cost:one(true),reason}),
};
interface Inputs {createGrant:T.CreateConfigurationGrant;revokeGrant:T.RevokeConfigurationGrant;createPlan:T.CreateConfigurationPlan;validatePlan:T.ValidateConfigurationPlan;approvePlan:T.ApproveConfigurationPlan;applyPlan:T.ApplyConfigurationPlan;verify:T.StartConfigurationVerification}
export function configurationInput<K extends keyof Inputs>(type:K,value:Inputs[K]):Inputs[K]&Record<string,unknown>{
 if(!Object.hasOwn(requestChecks,type)||!requestChecks[type](value))throw new TypeError('Invalid configuration input');
 const v=value as unknown as Record<string,unknown>;
 if(type==='createGrant'){
  if(Boolean(v.parent_grant_id)!==Boolean(v.expected_parent_version)||new Set(v.actions as string[]).size!==(v.actions as string[]).length||new Set((v.destinations as T.ConfigurationDestinationRef[]).map(x=>x.id)).size!==(v.destinations as unknown[]).length)throw new TypeError('Grant pins/actions must be distinct and parent version explicit');
 }
 if(type==='createPlan'){
  const ops=v.operations as T.ConfigurationOperationInput[];
  if(new Set(ops.map(x=>x.id)).size!==ops.length||new Set(ops.map(x=>x.destination_id)).size!==ops.length)throw new TypeError('Plan operation IDs/destinations must be distinct');
 }
 return value as Inputs[K]&Record<string,unknown>;
}
