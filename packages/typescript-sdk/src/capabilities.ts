// SPDX-License-Identifier: Apache-2.0
/** Public wire validation only. No authorization decisions or command execution. */
import { ProtocolError } from "./errors.js";
import { credentialFree } from "./security.js";
import type { ProductCapabilities } from "./capabilityTypes.js";

const surfaces = ["http", "cli", "python", "typescript", "mcp", "workspace"];
const rolePairs = new Set(["human:viewer", "human:member", "human:admin", "human:owner", "service:viewer", "service:member"]);
const effects = ["read_only", "platform_write", "queues_external_work", "external_dispatch", "records_observation"];
function fail(): never { throw new TypeError(); }
function record(v: unknown): Record<string, unknown> { return v !== null && typeof v === "object" && !Array.isArray(v) ? v as Record<string, unknown> : fail(); }
function text(v: unknown, max = 4096, empty = false): string {
  if (typeof v !== "string" || (!empty && !v.trim()) || v.includes("\0") || new TextEncoder().encode(v).length > max) fail();
  for (const c of v) { const cp = c.codePointAt(0)!; if (cp >= 0xd800 && cp <= 0xdfff) fail(); }
  return v;
}
function id(v: unknown): string { const s = text(v, 128); if (!/^[A-Za-z0-9][A-Za-z0-9_.:-]*$(?![\s\S])/.test(s)) fail(); return s; }
function list(v: unknown, min: number, max: number): unknown[] { return Array.isArray(v) && v.length >= min && v.length <= max ? v : fail(); }
function strings(v: unknown, min = 0, max = 32, ids = false): string[] { const a = list(v, min, max).map(x => ids ? id(x) : text(x)); if (ids && new Set(a).size !== a.length) fail(); return a; }
function choice(v: unknown, values: readonly string[]): void { if (typeof v !== "string" || !values.includes(v)) fail(); }
function unique(v: unknown[], key = "id"): Set<string> { const ids = v.map(x => id(record(x)[key])); if (new Set(ids).size !== ids.length) fail(); return new Set(ids); }
function docs(v: unknown): void {
  for (const item of list(v, 1, 8)) {
    const ref = text(item, 256);
    if (!/^(?:docs|contracts)\/[A-Za-z0-9_./-]+(?:#[A-Za-z0-9_.-]+)?$(?![\s\S])/.test(ref) || ref.split("#")[0]!.split("/").some(x => ["", ".", ".."].includes(x))) fail();
  }
}
function command(v: unknown): void {
  const args = list(v, 1, 20);
  if (args[0] !== "platform") fail();
  for (const item of args) {
    const arg = text(item, 512);
    if (!/^(?:[A-Za-z0-9_.,:{}=@+/-]+|<[A-Za-z0-9_.-]+>)$(?![\s\S])/.test(arg) || arg.startsWith("/") || arg.includes("://") || arg.split("/").includes("..")) fail();
  }
}

export function capabilityFacts(input: unknown, organizationId: string, principalId?: string): ProductCapabilities {
  try {
    const value = record(input);
    if (!credentialFree(value, 50000)) fail();
    const pending: unknown[] = [value];
    while (pending.length) {
      const item = pending.pop();
      if (Array.isArray(item)) pending.push(...item);
      else if (item && typeof item === "object") {
        for (const [key, child] of Object.entries(item)) {
          if (["canexecute", "executionpermit", "authorized"].includes(key.replace(/[^a-z0-9]/gi, "").toLowerCase())) fail();
          pending.push(child);
        }
      }
    }
    if (value.catalog_version !== "1" || value.api_contract !== "0.14.0" || value.assessment !== "support_and_role_observation" || value.read_only !== true) fail();
    const observed = text(value.observed_at, 128);
    if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z$(?![\s\S])/.test(observed) || !Number.isFinite(Date.parse(observed))) fail();
    // Date.parse normalizes impossible dates; compare the calendar portion as well.
    if (new Date(observed).toISOString().slice(0, 19) !== observed.slice(0, 19)) fail();
    const identity = record(value.identity);
    if (identity.organization_id !== organizationId || (principalId !== undefined && identity.principal_id !== principalId)) fail();
    id(identity.principal_id); text(identity.organization_id, 128);
    const identityPair = `${text(identity.principal_kind, 128)}:${text(identity.role, 128)}`;
    if (!rolePairs.has(identityPair)) fail();
    const permissions = strings(identity.permissions, 0, 64, true);
    strings(value.authorization_rules, 1, 32);
    const operations = list(value.operations, 1, 128);
    const operationIds = unique(operations), routes = new Set<string>();
    for (const item of operations) {
      const op = record(item); id(op.capability_id);
      choice(op.method, ["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]);
      const path = text(op.path, 512);
      if (!/^\/api\/v1(?:\/(?:[A-Za-z0-9_-]+|\{[A-Za-z][A-Za-z0-9_]*\}))+$(?![\s\S])/.test(path)) fail();
      const route = `${op.method} ${path.replace(/\{[^}]+\}/g, "{}")}`;
      if (routes.has(route)) fail(); routes.add(route);
      text(op.input_schema, 128, true); text(op.output_schema, 128);
      choice(op.response_kind, ["object", "list", "sse"]);
      if (!Number.isSafeInteger(op.success_status) || Number(op.success_status) < 200 || Number(op.success_status) > 299) fail();
      choice(op.access_model, ["public", "current_membership", "role_and_resource", "scoped_configuration", "human_owner"]);
      const alternatives = list(op.permission_alternatives, 0, 4).map(x => strings(x, 1, 4, true).sort().join("\0"));
      if (new Set(alternatives).size !== alternatives.length) fail();
      strings(op.runtime_guards, 1, 32); choice(op.effect, effects);
      choice(op.cost_semantics, ["none", "may_reserve_and_queue", "reserves_upper_bound", "possible_charge_existing_reservation", "observation_no_settlement"]);
      choice(op.retry_policy, ["read_with_current_authorization", "explicit_same_key_after_inspection", "scoped_stream_resume"]);
      const bindings = list(op.bindings, 6, 6), observedSurfaces = unique(bindings, "surface");
      if (!surfaces.every(x => observedSurfaces.has(x))) fail();
      for (const item of bindings) {
        const binding = record(item); choice(binding.status, ["available", "operator_opt_in", "not_exposed"]);
        text(binding.name, 512, binding.status === "not_exposed");
        if (binding.status === "not_exposed" && binding.name !== "") fail();
      }
      docs(op.documentation);
    }
    const profiles = list(value.profiles, 1, 32); unique(profiles);
    for (const item of profiles) {
      const profile = record(item); text(profile.name); text(profile.summary);
      choice(profile.status, ["implemented", "bounded", "not_implemented"]);
      strings(profile.constraints, 1, 32); docs(profile.documentation);
    }
    const pairs = new Set<string>();
    for (const item of list(value.roles, 6, 6)) {
      const role = record(item), pair = `${text(role.principal_kind, 128)}:${text(role.role, 128)}`;
      if (!rolePairs.has(pair) || pairs.has(pair)) fail(); pairs.add(pair);
      const rolePermissions = strings(role.permissions, 0, 64, true);
      if (pair === identityPair && !permissions.every(p => rolePermissions.includes(p))) fail();
    }
    const onboarding = list(value.onboarding, 6, 6); unique(onboarding);
    for (const item of onboarding) {
      const guide = record(item); text(guide.persona); text(guide.summary);
      choice(guide.status, ["available", "partial", "not_implemented"]);
      strings(guide.prerequisites, 1, 32); docs(guide.documentation);
      const steps = list(guide.steps, 0, 12); unique(steps);
      for (const item of steps) {
        const step = record(item); text(step.instruction); text(step.actor);
        if (!strings(step.operation_ids, 0, 16, true).every(x => operationIds.has(x))) fail();
        choice(step.effect, effects);
        if (typeof step.cost_acknowledgement !== "boolean") fail();
        for (const cmd of list(step.commands, 0, 4)) command(cmd);
      }
    }
    command(value.remediation_command);
    return value as unknown as ProductCapabilities;
  } catch { throw new ProtocolError("Invalid product capabilities response"); }
}
