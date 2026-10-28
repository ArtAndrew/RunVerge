// SPDX-License-Identifier: Elastic-2.0
import type {CurrentIdentity,ProductCapabilities,CapabilityOperation,CapabilityProfile,CapabilityRole,CapabilityOnboarding,CapabilityStep,CapabilityBinding} from '../../packages/typescript-sdk/src/types'
export type {ProductCapabilities,CapabilityOperation,CapabilityProfile,CapabilityRole,CapabilityOnboarding,CapabilityStep,CapabilityBinding}

const surfaces=['http','cli','python','typescript','mcp','workspace']
const effects=['read_only','platform_write','queues_external_work','external_dispatch','records_observation']
const record=(v:unknown):v is Record<string,unknown>=>Boolean(v&&typeof v==='object'&&!Array.isArray(v))
const encoder=new TextEncoder()
function text(v:unknown,max=4096,empty=false):v is string{return typeof v==='string'&&(empty||Boolean(v.trim()))&&!v.includes('\0')&&!/[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/u.test(v)&&encoder.encode(v).length<=max}
const list=(v:unknown,min:number,max:number,check:(v:unknown)=>boolean):v is unknown[]=>Array.isArray(v)&&v.length>=min&&v.length<=max&&v.every(check)
const strings=(v:unknown,min=0,max=32,size=4096):v is string[]=>list(v,min,max,x=>text(x,size))
const command=(v:unknown):v is string[]=>strings(v,1,20,512)&&v[0]==='platform'
const unique=(v:string[])=>new Set(v).size===v.length
const member=(v:unknown,values:string[])=>typeof v==='string'&&values.includes(v)
const docs=(v:unknown)=>list(v,1,8,x=>text(x,256)&&/^(?:docs|contracts)\/[A-Za-z0-9_./#-]+$/.test(x)&&!x.split('/').includes('..')&&!x.includes('//'))
const fail=()=>{throw new Error('Invalid product capability catalogue')}
const utcDate=(v:unknown):v is string=>text(v,128)&&/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?Z$/.test(v)&&Number.isFinite(Date.parse(v))&&new Date(v).toISOString().slice(0,19)===v.slice(0,19)

// Bound unknown extension fields as well as known arrays. Never retain private
// connection or credential descriptors accidentally added by a proxy/server.
function bounded(v:unknown){const pending:[unknown,number][]=[[v,0]];let nodes=0,bytes=0;const privateNames=new Set(['token','rawtoken','accesstoken','refreshtoken','idtoken','sessiontoken','secret','clientsecret','password','privatekey','apikey','authorization','credential','credentials','credentialreference','credentialhash','tokenhash','endpoint','environment'])
 while(pending.length){const [x,depth]=pending.pop()!;if(++nodes>100000||depth>20)fail();if(typeof x==='string'){if(x.length>4*1024*1024)fail();bytes+=encoder.encode(x).length;if(bytes>4*1024*1024)fail()}else if(x&&typeof x==='object'){if(Array.isArray(x)&&x.length>100000)fail();const entries=Object.entries(x);if(pending.length+nodes+entries.length>100000)fail();for(const [key,value]of entries){if(!Array.isArray(x)&&privateNames.has(key.replace(/[^a-z0-9]/gi,'').toLowerCase()))fail();pending.push([value,depth+1])}}}
}
function validIdentity(v:unknown,org:string):v is CurrentIdentity{return record(v)&&v.organization_id===org&&text(v.organization_id,128)&&text(v.principal_id,128)&&member(v.principal_kind,['human','service'])&&member(v.role,v.principal_kind==='human'?['owner','admin','member','viewer']:['member','viewer'])&&strings(v.permissions,0,64,128)&&unique(v.permissions)}
export function sameCapabilityActor(a:CurrentIdentity,b:CurrentIdentity){return a.organization_id===b.organization_id&&a.principal_id===b.principal_id&&a.principal_kind===b.principal_kind}
export function capabilityContext(identity:CurrentIdentity){return JSON.stringify([identity.organization_id,identity.principal_id,identity.principal_kind,identity.role,[...identity.permissions].sort()])}
function binding(v:unknown):v is CapabilityBinding{return record(v)&&member(v.surface,surfaces)&&member(v.status,['available','operator_opt_in','not_exposed'])&&text(v.name,512,v.status==='not_exposed')&&(v.status!=='not_exposed'||v.name==='')}
function operation(v:unknown):v is CapabilityOperation{return record(v)&&text(v.id,128)&&text(v.capability_id,128)&&member(v.method,['GET','POST','PUT','PATCH','DELETE','HEAD','OPTIONS'])&&text(v.path,512)&&/^\/api\/v1\/[A-Za-z0-9_{}./-]+$/.test(v.path)&&!v.path.split('/').includes('..')&&text(v.input_schema,128,true)&&text(v.output_schema,128)&&member(v.response_kind,['object','list','sse'])&&Number.isInteger(v.success_status)&&Number(v.success_status)>=200&&Number(v.success_status)<=299&&member(v.access_model,['public','current_membership','role_and_resource','scoped_configuration','human_owner'])&&list(v.permission_alternatives,0,4,g=>strings(g,1,4,128))&&strings(v.runtime_guards,1,32)&&member(v.effect,effects)&&member(v.cost_semantics,['none','may_reserve_and_queue','reserves_upper_bound','possible_charge_existing_reservation','observation_no_settlement'])&&member(v.retry_policy,['read_with_current_authorization','explicit_same_key_after_inspection','scoped_stream_resume'])&&list(v.bindings,6,6,binding)&&unique((v.bindings as CapabilityBinding[]).map(b=>b.surface))&&docs(v.documentation)}
function profile(v:unknown):v is CapabilityProfile{return record(v)&&text(v.id,128)&&text(v.name)&&member(v.status,['implemented','bounded','not_implemented'])&&text(v.summary)&&strings(v.constraints,1,32)&&docs(v.documentation)}
function role(v:unknown):v is CapabilityRole{return record(v)&&member(v.principal_kind,['human','service'])&&member(v.role,v.principal_kind==='human'?['owner','admin','member','viewer']:['member','viewer'])&&strings(v.permissions,0,64,128)&&unique(v.permissions)}
function step(v:unknown,ids:Set<string>):v is CapabilityStep{return record(v)&&text(v.id,128)&&text(v.instruction)&&text(v.actor)&&strings(v.operation_ids,0,16,128)&&v.operation_ids.every(id=>ids.has(id))&&list(v.commands,0,4,command)&&member(v.effect,effects)&&typeof v.cost_acknowledgement==='boolean'}
function onboarding(v:unknown,ids:Set<string>):v is CapabilityOnboarding{return record(v)&&text(v.id,128)&&text(v.persona)&&member(v.status,['available','partial','not_implemented'])&&text(v.summary)&&strings(v.prerequisites,1,32)&&list(v.steps,0,12,s=>step(s,ids))&&unique((v.steps as CapabilityStep[]).map(s=>s.id))&&docs(v.documentation)}

export function validateCapabilities(value:unknown,organization:string,expected?:CurrentIdentity):ProductCapabilities{
 bounded(value)
 if(!record(value)||value.catalog_version!=='1'||value.api_contract!=='0.14.0'||value.assessment!=='support_and_role_observation'||value.read_only!==true||!utcDate(value.observed_at)||!validIdentity(value.identity,organization)||!strings(value.authorization_rules,1,32)||!list(value.operations,1,128,operation)||!list(value.profiles,1,32,profile)||!list(value.roles,6,6,role)||!command(value.remediation_command))fail()
 const v=value as unknown as ProductCapabilities
 if(expected&&capabilityContext(v.identity)!==capabilityContext(expected))fail()
 if(!unique(v.operations.map(o=>o.id))||!unique(v.operations.map(o=>`${o.method} ${o.path}`))||!unique(v.profiles.map(p=>p.id))||!unique(v.roles.map(r=>`${r.principal_kind}:${r.role}`))||!list(v.onboarding,6,6,o=>onboarding(o,new Set(v.operations.map(o=>o.id))))||!unique(v.onboarding.map(o=>o.id)))fail()
 return v
}

export const capabilityRoleLabels:Record<string,string>={owner:'Владелец',admin:'Администратор',member:'Участник',viewer:'Наблюдатель'}
export const capabilityStatusLabels:Record<string,string>={implemented:'Поддерживается',bounded:'С ограничениями',not_implemented:'Недоступно',available:'Можно начать',partial:'Частично доступно',operator_opt_in:'Включает оператор',not_exposed:'Нет в этом интерфейсе'}
export const capabilityEffectLabels:Record<string,string>={read_only:'Только чтение',platform_write:'Сохраняет изменения',queues_external_work:'Ставит внешнюю работу в очередь',external_dispatch:'Отправляет внешний запрос',records_observation:'Записывает наблюдение'}
