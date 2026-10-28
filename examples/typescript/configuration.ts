// SPDX-License-Identifier: Apache-2.0
/** Inspect by default. Explicit plan mode creates and statically validates only. */
import {parseArgs} from 'node:util';
import {Client,AgentPlatformError} from '../../packages/typescript-sdk/dist/index.js';
const {values:v}=parseArgs({options:{help:{type:'boolean',short:'h'},url:{type:'string',default:process.env.AGENT_PLATFORM_URL??'http://127.0.0.1:8080'},'create-plan':{type:'string'},'destination-version':{type:'string'},'known-resource-version':{type:'string'},'grant-id':{type:'string'},'request-key':{type:'string'},reason:{type:'string'}}});
if(v.help){console.log('Inspect: node configuration.ts [--url URL]\nPlan + static validate only: --create-plan ID --destination-version N --known-resource-version N --request-key KEY --reason TEXT [--grant-id ID]\nNo approve/apply/verify, credential issuance, transport retry or paid work is automatic.');process.exit(0)}
function integer(value:string|undefined,minimum:number){if(value===undefined||!/^(0|[1-9][0-9]*)(?![\s\S])/.test(value)||Number(value)<minimum||Number(value)>2147483647)throw new TypeError('Exact bounded resource/destination version required');return Number(value)}
try{
 const planMode=v['create-plan']!==undefined;
 if(!planMode&&['destination-version','known-resource-version','grant-id','request-key','reason'].some(k=>v[k as keyof typeof v]!==undefined))throw new TypeError('Plan options require explicit --create-plan');
 const destinationVersion=planMode?integer(v['destination-version'],1):0,knownVersion=planMode?integer(v['known-resource-version'],0):0;
 if(planMode&&(!v['request-key']||v['request-key'].length>100||!/^[\x21-\x7e]+(?![\s\S])/.test(v['request-key'])||!v.reason?.trim()))throw new TypeError('Plan mode needs a stable 1..100 ASCII request key and a reason');
 const token=process.env.AGENT_PLATFORM_TOKEN,organizationId=process.env.AGENT_PLATFORM_ORGANIZATION_ID;if(!token||!organizationId)throw new TypeError('Set platform token and organization securely in the environment');
 const client=new Client({baseUrl:v.url!,token,organizationId}),catalog=await client.configurationCatalog();let destinations=await client.configurationDestinations();
 if(!planMode){console.log(JSON.stringify({catalog,destinations:destinations.data,next_cursor:destinations.next_cursor},null,2))}
 else{
  let destination;for(let page=0;page<20;page++){destination=destinations.data.find(d=>d.id===v['create-plan']&&d.version===destinationVersion&&d.active);if(destination||!destinations.next_cursor||page===19)break;destinations=await client.configurationDestinations({cursor:destinations.next_cursor})}if(!destination)throw new TypeError('Exact active destination not found in the first 20 pages; inspect through the SDK');
  const plan=await client.createConfigurationPlan({...(v['grant-id']?{grant_id:v['grant-id']}:{ }),base_revision:catalog.revision,ttl_seconds:600,operations:[{id:'setup',destination_id:destination.id,destination_version:destination.version,expected_resource_version:knownVersion,active:true}],reason:v.reason!},{idempotencyKey:v['request-key']+'-plan'});
  const validation=await client.validateConfigurationPlan(plan.id,{expected_version:plan.version,plan_digest:plan.digest},{idempotencyKey:v['request-key']+'-validate'});
  console.log(JSON.stringify({plan_id:plan.id,version:plan.version,digest:plan.digest,expires_at:plan.expires_at,validation_id:validation.id,valid:validation.valid,decision:validation.decision.outcome,obligations:validation.decision.obligations,reason_codes:validation.decision.reason_codes,applied:false,connection_checked:false},null,2));
 }
}catch(error){console.error(error instanceof AgentPlatformError?`${error.name}: request stopped; inspect current records before an explicit retry`:error instanceof TypeError?error.message:'Configuration example stopped');process.exitCode=1}
