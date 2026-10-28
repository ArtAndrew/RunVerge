// SPDX-License-Identifier: Apache-2.0
// Opt-in read-only check of SDK 0.5 against a running, authorized server.
import assert from "node:assert/strict";
import { Client } from "../../packages/typescript-sdk/dist/index.js";
import type { CostOptions, CostReport } from "../../packages/typescript-sdk/dist/index.js";

const taskId = process.argv[2];
if (!taskId) throw new Error("Pass the task ID as the only argument");
const client = new Client({
  baseUrl: process.env.AGENT_PLATFORM_URL ?? "http://127.0.0.1:8080",
  token: process.env.AGENT_PLATFORM_TOKEN!,
  organizationId: process.env.AGENT_PLATFORM_ORGANIZATION_ID!,
});
const task = await client.getTask(taskId);
const identity=await client.getCurrentIdentity();
assert.equal(identity.organization_id,task.organization_id);assert.ok(identity.principal_id);
assert.equal("token" in identity,false);
assert.equal(task.root_id, taskId, "fixture scenario expects a root task");
assert.equal(task.depth, 0);
assert.equal(task.financial_hold, false);
const counts: Record<string, number> = {};
const availableModels = (await client.models({ limit: 100 })).data.length;
for (const collection of ["runs", "operations", "actions", "approvals", "candidates", "decisions", "children"] as const) {
  const ids = new Set<string>();
  const cursors = new Set<string>();
  let cursor: string | undefined;
  do {
    assert.ok(cursors.size < 100, "bounded smoke pagination");
    const page = await client[collection](taskId, { limit: 1, ...(cursor === undefined ? {} : { cursor }) });
    for (const item of page.data) {
      if(collection === "children") assert.equal((item as {parent_id?:string}).parent_id,taskId);
      else assert.equal((item as {task_id:string}).task_id, taskId);
      assert.ok(!ids.has(item.id), "duplicate execution record across pages");
      ids.add(item.id);
    }
    cursor = page.next_cursor;
    if (cursor !== undefined) {
      assert.ok(!cursors.has(cursor), "repeated execution cursor");
      cursors.add(cursor);
    }
  } while (cursor !== undefined);
  counts[collection] = ids.size;
}
assert.ok(counts.runs! > 0 && counts.operations! > 0 && counts.candidates! > 0);
assert.equal(task.attempt_count,1);assert.equal(task.max_runs,3);assert.ok(task.current_run_id);
const candidate=(await client.candidates(taskId)).data.find(item=>item.id===task.candidate_id)!;
assert.equal(candidate.source,"fixture");assert.equal(candidate.run_id,task.current_run_id);assert.equal(task.accepted_decision_id,undefined);
const report = await client.costs(taskId, { limit: 1 });
assert.equal(report.spent_microusd, "1000", "fixture cost is synthetic");
assert.equal(report.reserved_microusd, "0");
assert.equal(report.unknown_microusd, "0");
assert.equal(report.financial_hold, false);
assert.equal(report.incomplete, false);
assert.equal(report.pending_quarantine_count, 0);
assert.equal(report.budget_microusd, task.budget_microusd);
const collections = [
  ["reservations", "next_reservation_cursor", "reservationCursor"],
  ["entries", "next_entry_cursor", "entryCursor"],
  ["incidents", "next_incident_cursor", "incidentCursor"],
  ["quarantines", "next_quarantine_cursor", "quarantineCursor"],
] as const;
for (const [collection, outputCursor, inputCursor] of collections) {
  const ids = new Set<string>();
  const cursors = new Set<string>();
  let page: CostReport = report;
  while (true) {
    for (const item of page[collection]) {
      assert.ok(!ids.has(item.id), "duplicate accounting record across pages");
      ids.add(item.id);
      if (collection === "entries") assert.equal(item.synthetic, true, "fixture ledger must remain labeled synthetic");
    }
    const cursor = page[outputCursor];
    if (cursor === undefined) break;
    assert.ok(!cursors.has(cursor) && cursors.size < 100, "bounded cost pagination");
    cursors.add(cursor);
    const options: CostOptions = { limit: 1, [inputCursor]: cursor };
    page = await client.costs(taskId, options);
  }
  counts[collection] = ids.size;
}
assert.ok(counts.reservations! > 0 && counts.entries! > 0);
console.log(JSON.stringify({ sdk: "typescript", task_id: taskId, counts, available_models: availableModels,
  budget_microusd: report.budget_microusd, synthetic_spent_microusd: report.spent_microusd,
  reserved_microusd: report.reserved_microusd, unknown_microusd: report.unknown_microusd,
  financial_hold: report.financial_hold }));
