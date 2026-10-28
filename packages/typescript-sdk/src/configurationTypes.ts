// SPDX-License-Identifier: Apache-2.0
// Public wire declarations only. Trusted host and worker profiles are private.
import type {ListOptions} from './types.js';
export type ConfigurationAction='inspect'|'plan'|'validate'|'apply'|'verify'|'delegate';
export type ConfigurationKind='model'|'mcp';
export interface ConfigurationDestinationRef {id:string;version:number}
export interface ConfigurationScope {destination_id:string;destination_version:number;kind:ConfigurationKind;target_id:string;credential_purpose_id:string;credential_purpose_version:number}
export interface ConfigurationDestination {id:string;organization_id:string;version:number;kind:ConfigurationKind;name:string;target_id:string;credential_purpose_id:string;credential_purpose_version:number;requires_approval:boolean;max_verify_charge_microusd:string;model?:string;discovery_unmetered_approved:boolean;active:boolean;created_at:string}
export interface ConfigurationCatalog {organization_id:string;revision:string;audience:'platform.configuration.v1';actions:ConfigurationAction[];can_issue_root_grants:boolean;can_approve:boolean;max_operations:number;max_grant_depth:number;available_commands:string[]}
export interface CapabilityEnvelope {id:string;organization_id:string;audience:'platform.configuration.v1';issuer_id:string;subject_id:string;parent_grant_id?:string;depth:number;version:number;actions:ConfigurationAction[];scopes:ConfigurationScope[];max_plans:number;used_plans:number;max_verifications:number;used_verifications:number;max_allocated_charge_microusd:string;allocated_charge_microusd:string;revoked:boolean;status:string;created_at:string;expires_at:string;available_commands:string[]}
export interface CreateConfigurationGrant {subject_id:string;parent_grant_id?:string;expected_parent_version?:number;actions:ConfigurationAction[];destinations:ConfigurationDestinationRef[];ttl_seconds:number;max_plans:number;max_verifications:number;max_allocated_charge_microusd:string;reason:string}
export interface RevokeConfigurationGrant {expected_version:number;reason:string}
export interface ConfigurationOperationInput {id:string;destination_id:string;destination_version:number;expected_resource_version:number;active:boolean}
export interface ConfigurationOperation {id:string;scope:ConfigurationScope;expected_resource_version:number;active:boolean}
export interface ConfigurationDiff {operation_id:string;kind:ConfigurationKind;target_id:string;expected_version:number;new_version:number;active:boolean;retired_versions:number[];may_stop_pinned_tasks:boolean}
export interface CreateConfigurationPlan {grant_id?:string;base_revision:string;ttl_seconds:number;operations:ConfigurationOperationInput[];reason:string}
export interface ConfigurationPlan {id:string;organization_id:string;actor_id:string;grant_id?:string;version:number;status:string;base_revision:string;digest:string;operations:ConfigurationOperation[];diff:ConfigurationDiff[];reason:string;created_at:string;expires_at:string;applied_at?:string;applied_revision:string;receipt_id?:string;available_commands:string[]}
export interface PolicyDecision {id:string;organization_id:string;action:string;target_id:string;outcome:string;reason_codes:string[];grant_id?:string;configuration_revision:string;obligations:string[];evaluated_at:string}
export interface ValidateConfigurationPlan {expected_version:number;plan_digest:string}
export interface ConfigurationValidation {id:string;organization_id:string;plan_id:string;plan_version:number;plan_digest:string;base_revision:string;current_revision:string;valid:boolean;decision:PolicyDecision;diff:ConfigurationDiff[];created_at:string}
export interface ApproveConfigurationPlan {expected_version:number;plan_digest:string;validation_id:string;reason:string}
export type ApplyConfigurationPlan=ApproveConfigurationPlan;
export interface ConfigurationApproval {id:string;organization_id:string;plan_id:string;plan_digest:string;validation_id:string;principal_id:string;reason:string;created_at:string}
export interface StartConfigurationVerification {expected_plan_version:number;plan_digest:string;operation_id:string;max_charge_microusd:string;acknowledge_cost:true;reason:string}
export interface ConfigurationVerification {id:string;organization_id:string;plan_id:string;operation_id:string;actor_id:string;grant_id?:string;scope:ConfigurationScope;resource_version:number;kind:ConfigurationKind;status:string;version:number;task_id?:string;max_charge_microusd:string;cost_status:string;protocol_version?:string;tool_count:number;catalog_digest?:string;response_digest?:string;reason_code?:string;created_at:string;updated_at:string}
export interface ConfigurationResource {organization_id:string;kind:ConfigurationKind;id:string;version:number;scope:ConfigurationScope;active:boolean;created_at:string}
export interface ConfigurationAuditEntry {id:string;organization_id:string;actor_type:string;actor_id?:string;object_type:string;object_id:string;action:string;digest:string;reason:string;created_at:string}
export interface ConfigurationVerificationOptions extends ListOptions {planId?:string}
