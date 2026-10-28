// SPDX-License-Identifier: Apache-2.0
/** Descriptive catalogue wire types. These are never execution permissions. */
import type { CurrentIdentity } from "./types.js";

export type CapabilityEffect = "read_only" | "platform_write" | "queues_external_work" | "external_dispatch" | "records_observation";
export type CapabilitySurface = "http" | "cli" | "python" | "typescript" | "mcp" | "workspace";
export interface CapabilityBinding { surface: CapabilitySurface; status: "available" | "operator_opt_in" | "not_exposed"; name: string }
export interface CapabilityOperation {
  id: string; capability_id: string; method: "GET" | "POST" | "PUT" | "PATCH" | "DELETE" | "HEAD" | "OPTIONS";
  path: string; input_schema: string; output_schema: string; response_kind: "object" | "list" | "sse"; success_status: number;
  access_model: "public" | "current_membership" | "role_and_resource" | "scoped_configuration" | "human_owner";
  permission_alternatives: string[][]; runtime_guards: string[]; effect: CapabilityEffect;
  cost_semantics: "none" | "may_reserve_and_queue" | "reserves_upper_bound" | "possible_charge_existing_reservation" | "observation_no_settlement";
  retry_policy: "read_with_current_authorization" | "explicit_same_key_after_inspection" | "scoped_stream_resume";
  bindings: CapabilityBinding[]; documentation: string[];
}
export interface CapabilityProfile { id: string; name: string; status: "implemented" | "bounded" | "not_implemented"; summary: string; constraints: string[]; documentation: string[] }
export interface CapabilityRole { principal_kind: "human" | "service"; role: "viewer" | "member" | "admin" | "owner"; permissions: string[] }
export interface CapabilityStep { id: string; instruction: string; actor: string; operation_ids: string[]; commands: string[][]; effect: CapabilityEffect; cost_acknowledgement: boolean }
export interface CapabilityOnboarding { id: string; persona: string; status: "available" | "partial" | "not_implemented"; summary: string; prerequisites: string[]; steps: CapabilityStep[]; documentation: string[] }
export interface ProductCapabilities {
  catalog_version: "1"; api_contract: "0.14.0"; assessment: "support_and_role_observation"; read_only: true;
  observed_at: string; identity: CurrentIdentity; authorization_rules: string[]; operations: CapabilityOperation[];
  profiles: CapabilityProfile[]; roles: CapabilityRole[]; onboarding: CapabilityOnboarding[]; remediation_command: string[];
}
