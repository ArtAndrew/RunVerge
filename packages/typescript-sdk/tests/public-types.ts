// SPDX-License-Identifier: Apache-2.0
// Compile-only consumer test. Import public declarations, never SDK source.
import type {
  ProductCapabilities, CapabilityOperation, CapabilityProfile, CapabilityRole, CapabilityOnboarding, CapabilityStep,
  Material, MaterialContent, MaterialRevision, CreateMaterialInput, ReviseMaterialInput, PublishMaterialInput, Artifact, Action, Approval, AvailableModel, Client, CostOptions, CostQuarantine, CostReport, FinancialIncident, Money,
  LedgerEntry, Operation, Page, Reservation, Run, Task, TaskCandidate, TaskDecision, TeamDefinition, ReviewTaskInput, RetryTaskInput,
} from "../dist/index.js";

// Contract 0.3 deliberately uses exact decimal strings for financial facts.
const task: Task = {
  id: "task", organization_id: "org", owner_id: "principal", agent_id: "agent",
  agent_version: 1, goal: "Fixture", status: "queued", budget_microusd: 1000,
  reserved_microusd: "0", spent_microusd: "0", version: 1,
  root_id: "task", depth: 0, financial_hold: false,
  attempt_count: 1, max_runs: 3, execution_deadline: "2026-10-24T10:00:00Z",
  created_at: "2026-10-23T10:00:00Z", updated_at: "2026-10-23T10:00:00Z",
};

async function publicTypes(client: Client): Promise<void> {
  const support: ProductCapabilities = await client.capabilities({signal:new AbortController().signal});
  const readonlyObservation: true = support.read_only;
  const supportOperations: CapabilityOperation[] = support.operations;
  const profiles: CapabilityProfile[] = support.profiles;
  const roles: CapabilityRole[] = support.roles;
  const guides: CapabilityOnboarding[] = support.onboarding;
  const steps: CapabilityStep[] = guides.flatMap(guide => guide.steps);
  // @ts-expect-error Catalogue support never provides an execution authority flag.
  const permit = support.can_execute;
  void [readonlyObservation,supportOperations,profiles,roles,guides,steps,permit];
  const draft: CreateMaterialInput = {name:"result.md",mediaType:"text/markdown",content:""};
  const edit: ReviseMaterialInput = {expectedVersion:1,content:"Exact text",reason:"Edit",mergeBaseVersion:1};
  const publication: PublishMaterialInput = {expectedTaskVersion:1,expectedMaterialVersion:2,contentDigest:"a".repeat(64),parentCandidateId:"original"};
  const material: Material = await client.createMaterial(task.id,draft,{idempotencyKey:"draft"});
  const revised: Material = await client.reviseMaterial(task.id,material.id,edit,{idempotencyKey:"edit"});
  const materialPage: Page<Material> = await client.materials(task.id);
  const revisionPage: Page<MaterialRevision> = await client.materialRevisions(task.id,material.id);
  const exactVersion: MaterialContent = await client.getMaterialVersion(task.id,material.id,1);
  const exactArtifact: Artifact = await client.getArtifact(task.id,"artifact",{version:1});
  const proposed: Task = await client.publishMaterial(task.id,material.id,publication,{idempotencyKey:"publish"});
  // @ts-expect-error Publication needs an exact parent candidate.
  const unbound: PublishMaterialInput = {expectedTaskVersion:1,expectedMaterialVersion:2,contentDigest:"a".repeat(64)};
  void [revised,materialPage,revisionPage,exactVersion,exactArtifact,proposed,unbound];
  const team: TeamDefinition = {pattern:"delegate_verify",join_policy:"all",max_depth:2,max_children:2,
    members:[{id:"author",agent_id:"author-id",agent_version:2,task:"Draft"},{id:"verifier",agent_id:"verifier-id",agent_version:1}]};
  const candidates: Page<TaskCandidate> = await client.candidates(task.id);
  const decisions: Page<TaskDecision> = await client.decisions(task.id);
  const children: Page<Task> = await client.children(task.id);
  const review: ReviewTaskInput = {expectedVersion:1,candidateId:"candidate",artifactId:"artifact",artifactVersion:1,contentDigest:"a".repeat(64),decision:"request_changes",reason:"Cite sources"};
  const retry: RetryTaskInput = {expectedVersion:2,reason:"Revise",feedback:"Cite both conclusions",acknowledgeNewCost:true};
  // @ts-expect-error Explicit new-cost acknowledgment cannot be omitted.
  const missingAck: RetryTaskInput = {expectedVersion:2,reason:"Revise"};
  // @ts-expect-error False is not valid acknowledgment.
  const falseAck: RetryTaskInput = {expectedVersion:2,reason:"Revise",acknowledgeNewCost:false};
  await client.reviewTask(task.id,review,{idempotencyKey:"review-1"});
  await client.retryTask(task.id,retry,{idempotencyKey:"attempt-2"});
  void [team,candidates,decisions,children,missingAck,falseAck];
  const options: CostOptions = { entryCursor: "opaque", quarantineCursor: "opaque-q", limit: 1 };
  const models: Page<AvailableModel> = await client.models({limit: 1});
  const runs: Page<Run> = await client.runs(task.id);
  const operations: Page<Operation> = await client.operations(task.id);
  const actions: Page<Action> = await client.actions(task.id);
  const approvals: Page<Approval> = await client.approvals(task.id);
  const report: CostReport = await client.costs(task.id, options);
  const reservations: Reservation[] = report.reservations;
  const entries: LedgerEntry[] = report.entries;
  const incidents: FinancialIncident[] = report.incidents;
  const quarantines: CostQuarantine[] = report.quarantines;
  const exact: Money = report.spent_microusd;
  // @ts-expect-error Financial facts are never JS numbers in 0.3.
  const unsafe: Money = 1000;
  void [models, exact, unsafe, quarantines, runs, operations, actions, approvals, reservations, entries, incidents];
}
void publicTypes;

async function currentIdentityConsumer(client: import("../dist/index.js").Client) {
 const identity: import("../dist/index.js").CurrentIdentity = await client.getCurrentIdentity();
 const permissions: string[] = identity.permissions;
 return [identity.principal_id, identity.organization_id, permissions];
}
void currentIdentityConsumer;

async function exactToolConsumer(client: Client) {
 const tools: Page<import('../dist/index.js').ToolInfo> = await client.tools({limit:1});
 const executions: Page<import('../dist/index.js').ToolExecution> = await client.toolExecutions(task.id);
 const prepared: import('../dist/index.js').ToolExecutionView = await client.prepareToolExecution(task.id,{occurrenceId:'intent',expectedTaskVersion:1,candidateId:'candidate',artifactId:'artifact',artifactVersion:1,contentDigest:'a'.repeat(64),toolId:'tool',toolVersion:1,argumentsJson:'{"n":9007199254740993}',expiresInSeconds:900},{idempotencyKey:'prepare'});
 const view = await client.getToolExecution(task.id,prepared.execution.id);
 const cas = {expectedTaskVersion:view.task.version,expectedExecutionVersion:view.execution.version,expectedApprovalVersion:view.approval.version,intentHash:view.execution.intent_hash};
 await client.decideToolExecution(task.id,view.execution.id,{...cas,decision:'approve',reason:'Exact intent reviewed'},{idempotencyKey:'decision'});
 // @ts-expect-error Dispatch requires an explicit true acknowledgment.
 const invalid: import('../dist/index.js').DispatchToolExecutionInput = {...cas,acknowledgeCost:false};
 await client.dispatchToolExecution(task.id,view.execution.id,{...cas,acknowledgeCost:true},{idempotencyKey:'dispatch'});
 await client.reconcileToolExecution(task.id,view.execution.id,{expectedTaskVersion:view.task.version,expectedExecutionVersion:view.execution.version,intentHash:view.execution.intent_hash,outcome:'rejected',effectReference:'ref',result:'',evidenceDigest:'a'.repeat(64),reason:'Operator observation'},{idempotencyKey:'assertion'});
 return [tools,executions,view,invalid];
}
void exactToolConsumer;

async function nativeModelConsumer(client:Client){
 const proposals:Page<import('../dist/index.js').ModelToolProposal>=await client.modelProposals(task.id);
 const exact=await client.getModelProposal(task.id,'proposal');
 const binding:import('../dist/index.js').ModelToolResultBinding={proposal_id:exact.id,execution_id:'execution',execution_version:3,status:'denied'};
 const input:import('../dist/index.js').ContinueModelInput={expectedTaskVersion:1,candidateId:exact.candidate_id,artifactId:exact.artifact_id,artifactVersion:exact.artifact_version,contentDigest:exact.content_digest,results:[binding],acknowledgeCost:true,reason:'Continue with denied tool'};
 // @ts-expect-error A continuation always needs explicit acknowledgment of new model costs.
 const missing:import('../dist/index.js').ContinueModelInput={...input,acknowledgeCost:false};
 const next:Task=await client.continueModel(task.id,input,{idempotencyKey:'next-model-turn'});
 return[proposals,next,missing];
}
void nativeModelConsumer;

async function automationConsumer(client:Client){
 const state:import('../dist/index.js').TaskAutomationState=await client.automation(task.id);
 const input:import('../dist/index.js').CreateTaskAutomationInput={expectedTaskVersion:1,mode:'read_only',toolPins:[{tool_id:'tool',tool_version:1,tool_profile_digest:'a'.repeat(64)}],maxModelTurns:4,maxToolExecutions:8,maxChargeMicrousd:'1000',expiresInSeconds:900,acknowledgeCost:true,reason:'Delegate bounded read'};
 // @ts-expect-error Delegation requires an explicit true cost acknowledgment.
 const implicit:import('../dist/index.js').CreateTaskAutomationInput={...input,acknowledgeCost:false};
 // @ts-expect-error Delegation money is an exact decimal string.
 const numeric:import('../dist/index.js').CreateTaskAutomationInput={...input,maxChargeMicrousd:1000};
 await client.createAutomation(task.id,input,{idempotencyKey:'grant'});
 if(state.grant)await client.revokeAutomation(task.id,{grantId:state.grant.id,expectedVersion:state.grant.version,reason:'Stop'},{idempotencyKey:'revoke'});
 const signals:Page<import('../dist/index.js').ProgressSignal>=await client.progressSignals(task.id,{limit:20});
 return[state,signals,implicit,numeric];
}
void automationConsumer;

async function membershipsConsumer(client:Client){
 const caps:import('../dist/index.js').MembershipCapabilities=await client.membershipCapabilities();
 const members:Page<import('../dist/index.js').OrganizationMember>=await client.listMembers({limit:1});
 const created=await client.createMember({displayName:'Local',role:'viewer',reason:'Provision'},{idempotencyKey:'member-create'});
 await client.updateMember(created.principal_id,{expectedVersion:created.version,displayName:'Local',role:'member',active:false,reason:'Disable'},{idempotencyKey:'member-update'});
 await client.revokeMemberCredentials(created.principal_id,{expectedVersion:created.version,reason:'Revoke old org logins'},{idempotencyKey:'member-revoke'});
 const audit:Page<import('../dist/index.js').MembershipAuditEntry>=await client.listMembershipAudit({principalId:created.principal_id});
 // @ts-expect-error Active must be explicitly supplied even when false.
 const missingStatus:import('../dist/index.js').UpdateMemberInput={expectedVersion:1,displayName:'Local',role:'member',reason:'Review'};
 // @ts-expect-error Caller-supplied identity or email is not a provisioning input.
 const claimedIdentity:import('../dist/index.js').CreateMemberInput={displayName:'Local',role:'member',reason:'Review',principalId:'existing'};
 void[caps,members,audit,missingStatus,claimedIdentity];
}
void membershipsConsumer;

import type {OrganizationLoginPolicy,ServiceAccount,ServiceKey,OrganizationSecurityAuditEntry,UpdateLoginPolicyInput,CreateServiceAccountInput} from "../dist/index.js";
declare const securityPolicy:OrganizationLoginPolicy;declare const serviceAccount:ServiceAccount;declare const serviceKey:ServiceKey;declare const securityAudit:OrganizationSecurityAuditEntry;
const policyChange:UpdateLoginPolicyInput={expectedVersion:1,mode:"mixed",oidcIssuer:"",serviceAccessEnabled:false,maxServiceKeyTTLSeconds:3600,reason:"Disable apps"};
// @ts-expect-error Explicit application boolean is required.
const missingServiceFlag:UpdateLoginPolicyInput={expectedVersion:1,mode:"mixed",oidcIssuer:"",maxServiceKeyTTLSeconds:3600,reason:"Review"};
// @ts-expect-error Service accounts cannot have owner role.
const serviceOwner:CreateServiceAccountInput={displayName:"Integration",role:"owner",reason:"Invalid"};
void [securityPolicy,serviceAccount,serviceKey,securityAudit,policyChange,missingServiceFlag,serviceOwner];


async function streamConsumer(client: import("../dist/index.js").Client, signal: AbortSignal) {
 const snapshot: import("../dist/index.js").TaskEventSnapshot = await client.getTaskEventSnapshot("task", {signal});
 for await (const frame of client.streamTaskEvents("task", {cursor:snapshot.cursor,signal})) {
  if(frame.kind==='error') { const code:string=frame.error.code; void code; }
  else for(const event of frame.batch.events) {
   const sequence:string=event.sequence;void sequence;
   if(event.payload_version===0) {const raw:string=event.legacy_data_json;void raw;}
   else if(event.type==='action.dispatched') {const fence:string=event.data.fencing_token;void fence;}
  }
 }
 // @ts-expect-error exact decimal sequences cannot be coerced from Number
 await client.events("task", {after:9007199254740993});
}
void streamConsumer;

async function configurationConsumer(client:Client){
 const catalog:import('../dist/index.js').ConfigurationCatalog=await client.configurationCatalog();
 const input:import('../dist/index.js').CreateConfigurationPlan={base_revision:catalog.revision,ttl_seconds:900,operations:[{id:'op',destination_id:'approved',destination_version:1,expected_resource_version:0,active:false}],reason:'Explicit setup'};
 const plan=await client.createConfigurationPlan(input,{idempotencyKey:'plan'});
 const validation=await client.validateConfigurationPlan(plan.id,{expected_version:plan.version,plan_digest:plan.digest},{idempotencyKey:'validation'});
 await client.applyConfigurationPlan(plan.id,{expected_version:plan.version,plan_digest:plan.digest,validation_id:validation.id,reason:'Apply reviewed plan'},{idempotencyKey:'apply'});
 // @ts-expect-error configuration revisions and money are exact decimal strings.
 const numeric:import('../dist/index.js').CreateConfigurationPlan={...input,base_revision:9007199254740993};
 // @ts-expect-error private host endpoints are not public inputs.
 const endpoint:import('../dist/index.js').CreateConfigurationPlan={...input,endpoint:'https://private'};
 // @ts-expect-error explicit true cost acknowledgement is mandatory.
 const noAck:import('../dist/index.js').StartConfigurationVerification={expected_plan_version:2,plan_digest:plan.digest,operation_id:'op',max_charge_microusd:'1000',acknowledge_cost:false,reason:'Check'};
 void [numeric,endpoint,noAck];
}
void configurationConsumer;
