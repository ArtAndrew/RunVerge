// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { once } from "node:events";
import { readFile } from "node:fs/promises";
import { after, afterEach, before, describe, test } from "node:test";
import { APIError, Client, ProtocolError, TransportError } from "../dist/index.js";

const execution = JSON.parse(await readFile(new URL("../../../tests/sdk/execution-fixtures.json", import.meta.url), "utf8"));

describe("HTTP contract 0.14.0", { concurrency: false }, () => {
  const responses = [];
  const requests = [];
  let server;
  let baseUrl;

  before(async () => {
    server = createServer(async (req, res) => {res.setHeader("X-Platform-API-Version","0.14.0");
      const chunks = [];
      for await (const chunk of req) chunks.push(chunk);
      const text = Buffer.concat(chunks).toString("utf8");
      requests.push({ method: req.method, path: req.url, headers: req.headers,
                      body: text ? JSON.parse(text) : undefined });
      const response = responses.shift();
      if (!response) {
        res.writeHead(500).end("Unexpected HTTP request");
        return;
      }
      response(req, res);
    });
    server.listen(0, "127.0.0.1");
    await once(server, "listening");
    baseUrl = `http://127.0.0.1:${server.address().port}`;
  });

  after(async () => {
    server.closeAllConnections();
    server.close();
    await once(server, "close");
  });

  afterEach(() => {
    assert.equal(responses.length, 0, "unused response fixture");
    assert.equal(requests.length, 0, "unexpected or unasserted HTTP request");
  });

  function client(options = {}) {
    return new Client({ baseUrl, token: "private-test-token", organizationId: "org-1", ...options });
  }

  function respond(data, status = 200, headers = {}) {
    responses.push((_req, res) => {
      const body = Buffer.isBuffer(data) ? data : Buffer.from(JSON.stringify(data));
      res.writeHead(status, { "content-type": "application/json", "content-length": body.length, ...headers });
      res.end(body);
    });
  }

  test("createTask maps fields and sends scoped authentication and idempotency headers", async () => {
    respond({ data: { id: "task-1", reserved_microusd: "0", spent_microusd: "0", extra: true } }, 201);
    const result = await client().createTask({ goal: "Подготовить отчёт", agentId: "agent-1", budgetMicrousd: 1000 },
                                             { idempotencyKey: "create-task-1" });
    assert.equal(result.id, "task-1");
    assert.equal(result.extra, true);
    const request = requests.shift();
    assert.equal(request.method, "POST");
    assert.equal(request.path, "/api/v1/tasks");
    assert.deepEqual(request.body, { goal: "Подготовить отчёт", agent_id: "agent-1", budget_microusd: 1000 });
    assert.equal(request.headers.authorization, "Bearer private-test-token");
    assert.equal(request.headers["x-organization-id"], "org-1");
    assert.equal(request.headers["idempotency-key"], "create-task-1");
    assert.equal(request.headers["content-type"], "application/json");
    assert.ok(!JSON.stringify(client()).includes("private-test-token"));
  });

  test("token callback is invoked for every request", async () => {
    let generation = 0;
    const sdk = client({ token: async () => `token-${++generation}` });
    for (let index = 1; index <= 2; index++) {
      respond({ data: [] });
      await sdk.listAgents();
      assert.equal(requests.shift().headers.authorization, `Bearer token-${index}`);
    }
  });

  test("opaque list cursors round trip without interpreting their contents", async () => {
    const cursor = "123+456/789==";
    respond({ data: [{ id: "task-1", reserved_microusd: "0", spent_microusd: "0" }], next_cursor: cursor });
    const page = await client().listTasks({ limit: 50 });
    assert.equal(page.next_cursor, cursor);
    assert.equal(requests.shift().path, "/api/v1/tasks?limit=50");
    respond({ data: [] });
    const empty = await client().listTasks({ cursor: page.next_cursor });
    assert.equal(empty.next_cursor, undefined);
    const url = new URL(requests.shift().path, baseUrl);
    assert.equal(url.searchParams.get("cursor"), cursor);
  });

  test("agent creation and publication use the inspected version", async () => {
    const sdk = client();
    respond({ data: { id: "agent-1", version: 2 } }, 201);
    const draft = await sdk.createAgent({ name: "Fixture", instructions: "Exercise the API",
                                          model: "fixture:echo", tools: [], budgetMicrousd: 10_000 },
                                        { idempotencyKey: "agent-1" });
    assert.deepEqual(requests.shift().body, { name: "Fixture", instructions: "Exercise the API",
                                             model: "fixture:echo", tools: [], budget_microusd: 10_000 });
    respond({ data: { ...draft, published: true } });
    await sdk.publishAgent(draft.id, { expectedVersion: draft.version, idempotencyKey: "publish-1" });
    const publish = requests.shift();
    assert.equal(publish.path, "/api/v1/agents/agent-1/publish");
    assert.deepEqual(publish.body, { expected_version: 2 });
    assert.equal(publish.headers["idempotency-key"], "publish-1");
  });

  test("getTask, cancelTask, and artifacts use the documented routes", async () => {
    respond({ data: { id: "task 1", reserved_microusd: "0", spent_microusd: "0" } });
    await client().getTask("task 1");
    assert.equal(requests.shift().path, "/api/v1/tasks/task%201");
    respond({ data: { id: "task-1", reserved_microusd: "0", spent_microusd: "0", status: "cancelled" } });
    await client().cancelTask("task-1", { idempotencyKey: "cancel-1" });
    const cancel = requests.shift();
    assert.equal(cancel.path, "/api/v1/tasks/task-1/cancel");
    assert.deepEqual(cancel.body, {});
    assert.equal(cancel.headers["idempotency-key"], "cancel-1");
    respond({ data: [{ id: "artifact-1", content: "Fixture result" }] });
    assert.equal((await client().artifacts("task-1")).data[0].content, "Fixture result");
    assert.equal(requests.shift().path, "/api/v1/tasks/task-1/artifacts");
  });

  test("events poll with decimal string sequence and permit sequence gaps", async () => {
    respond({ data: [{id:"e-12",organization_id:"org-1",task_id:"task-1",sequence:"12",type:"task.completed",payload_version:0,legacy_data_json:"{}",created_at:"2026-10-24T00:00:00Z"}] });
    const events = await client().events("task-1", { after: "8", limit: 100 });
    assert.equal(events.data[0].sequence, "12");
    const request = requests.shift();
    assert.equal(request.path, "/api/v1/tasks/task-1/events?after=8&limit=100");
    assert.equal(request.headers.accept, "application/json");
  });

  test("execution pages preserve opaque cursors and scoped GET requests", async () => {
    const cursor = "123+456/789==";
    const sdk = client();
    for (const collection of ["runs", "operations", "actions", "approvals"]) {
      respond({ data: [execution[collection]], next_cursor: cursor });
      const page = await sdk[collection]("task 1", { limit: 1 });
      assert.deepEqual(page.data, [execution[collection]]);
      assert.equal(page.next_cursor, cursor);
      const request = requests.shift();
      assert.equal(request.method, "GET");
      assert.equal(request.path, `/api/v1/tasks/task%201/${collection}?limit=1`);
      assert.equal(request.headers.authorization, "Bearer private-test-token");
      assert.equal(request.headers["x-organization-id"], "org-1");
      assert.equal(request.headers["idempotency-key"], undefined);
      assert.equal(request.body, undefined);
      respond({ data: [] });
      assert.equal((await sdk[collection]("task-1", { cursor: page.next_cursor })).next_cursor, undefined);
      assert.equal(new URL(requests.shift().path, baseUrl).searchParams.get("cursor"), cursor);
    }
  });

  test("costs preserve summary totals, signed deltas, and independent cursors", async () => {
    respond({ data: execution.costs });
    const report = await client().costs("task 1", { limit: 1 });
    assert.deepEqual(report, execution.costs);
    assert.equal(report.entries[0].delta_spent_microusd, "-100");
    assert.equal(report.entries[0].synthetic, true);
    assert.equal(report.financial_hold, true);
    assert.equal(report.spent_microusd, "9007199254740993");
    assert.equal(report.quarantines[0].actual_microusd, "9223372036854775807");
    assert.equal(report.pending_quarantine_count, 1);
    assert.equal(report.incomplete, true);
    const request = requests.shift();
    assert.equal(request.path, "/api/v1/tasks/task%201/costs?limit=1");
    assert.equal(request.method, "GET");
    assert.equal(request.headers["idempotency-key"], undefined);
    respond({ data: execution.costs });
    await client().costs("task-1", { limit: 100,
      reservationCursor: report.next_reservation_cursor,
      entryCursor: report.next_entry_cursor, incidentCursor: report.next_incident_cursor,
      quarantineCursor: report.next_quarantine_cursor });
    const query = new URL(requests.shift().path, baseUrl).searchParams;
    assert.equal(query.get("limit"), "100");
    assert.equal(query.get("reservation_cursor"), report.next_reservation_cursor);
    assert.equal(query.get("entry_cursor"), report.next_entry_cursor);
    assert.equal(query.get("incident_cursor"), report.next_incident_cursor);
    assert.equal(query.get("quarantine_cursor"), report.next_quarantine_cursor);
  });

  test("costs reject malformed reports and summary numbers", async () => {
    for (const patch of [{ reservations: null }, { entries: [1] }, { incidents: {} },
      { spent_microusd: 1.5 }, { unknown_microusd: -1 }, { budget_microusd: true },
      { unknown_reservation_count: 0 }, { unknown_reservation_count: false }, { unknown_reservation_count: "01" }, { unknown_reservation_count: "9223372036854775808" },
      { financial_hold: "false" }, { next_entry_cursor: 1 }, { next_reservation_cursor: null }, { quarantines: null }, { pending_quarantine_count: -1 },
      { pending_quarantine_count: true }, { incomplete: "false" }, { spent_microusd: "9223372036854775808" },
      { spent_microusd: "0\n" }, { spent_microusd: "01" }, { next_quarantine_cursor: 1 }]) {
      respond({ data: { ...execution.costs, ...patch } });
      await assert.rejects(client().costs("task-1"), ProtocolError);
      requests.shift();
    }
    respond(Buffer.from('{"data":' + JSON.stringify(execution.costs).replace('"spent_microusd":"9007199254740993"', '"spent_microusd":9007199254740993') + '}'));
    await assert.rejects(client().costs("task-1"), ProtocolError);
    requests.shift();
  });

  test("available models use authorized GET and opaque cursor pagination", async () => {
    respond({ data: [execution.models], next_cursor: "model:next+/==" });
    const page = await client().models({limit: 1});
    assert.deepEqual(page.data, [execution.models]);
    const first = requests.shift();
    assert.equal(first.path, "/api/v1/models?limit=1");
    assert.equal(first.method, "GET");
    assert.equal(first.headers["idempotency-key"], undefined);
    respond({data: []});
    await client().models({cursor: page.next_cursor});
    assert.equal(new URL(requests.shift().path, baseUrl).searchParams.get("cursor"), page.next_cursor);
  });

  test("local model metadata is explicit and cannot hide an invalid tariff", async()=>{
    const base=execution.models;
    respond({data:[{...base,accounting_mode:'operator_unmetered_local',max_charge_microusd:0}]});
    assert.equal((await client().models()).data[0].accounting_mode,'operator_unmetered_local');requests.shift();
    for(const patch of[{accounting_mode:'free'},{accounting_mode:'metered',max_charge_microusd:0},{accounting_mode:'operator_unmetered_local',max_charge_microusd:1},{timeout_milliseconds:0},{max_input_tokens:true}]){
      respond({data:[{...base,...patch}]});await assert.rejects(client().models(),ProtocolError);requests.shift();
    }
    respond({data:{...execution.costs,unknown_microusd:'0',unknown_reservation_count:'9007199254740993'}});
    assert.equal((await client().costs('task-1')).unknown_reservation_count,'9007199254740993');requests.shift();
  });

  test("Task reads require contract 0.3 financial strings without numeric fallback", async () => {
    for (const invalid of [0, null, "01", "-1", "9223372036854775808"]) {
      respond({data: {id: "task-1", spent_microusd: invalid, reserved_microusd: "0"}});
      await assert.rejects(client().getTask("task-1"), ProtocolError);
      requests.shift();
    }
    respond({data: [{id: "task-1", spent_microusd: 1000, reserved_microusd: 0}]});
    await assert.rejects(client().listTasks(), ProtocolError);
    requests.shift();
    respond({data: {id: "task-1", spent_microusd: "9223372036854775807", reserved_microusd: "0"}});
    assert.equal((await client().getTask("task-1")).spent_microusd, "9223372036854775807");
    requests.shift();
  });

  test("nested report money refuses numeric and noncanonical wire values", async () => {
    for (const [collection, field] of [["reservations", "amount_microusd"], ["entries", "delta_spent_microusd"], ["incidents", "amount_microusd"], ["quarantines", "actual_microusd"]]) {
      for (const invalid of [1000, "01", "-0", "0\n", "9223372036854775808"]) {
        const report = structuredClone(execution.costs);
        report[collection][0][field] = invalid;
        respond({data: report});
        await assert.rejects(client().costs("task-1"), ProtocolError);
        requests.shift();
      }
    }
  });

  test("new reads preserve server errors without retry or fallback", async () => {
    const sdk = client();
    for (const collection of ["runs", "operations", "actions", "approvals", "costs"]) {
      respond({ error: { code: "not_found", message: "Not found", request_id: "req-hidden", retryable: false } }, 404);
      await assert.rejects(sdk[collection]("task-hidden"), error => error instanceof APIError && error.code === "not_found");
      assert.equal(requests.shift().method, "GET");
    }
  });

  test("APIError preserves structured metadata without retrying", async () => {
    respond({ error: { code: "unavailable", message: "Try later", request_id: "req-12", retryable: true } }, 503);
    await assert.rejects(client().listTasks(), error => {
      assert.ok(error instanceof APIError);
      assert.equal(error.statusCode, 503);
      assert.equal(error.code, "unavailable");
      assert.equal(error.message, "Try later");
      assert.equal(error.requestId, "req-12");
      assert.equal(error.retryable, true);
      return true;
    });
    requests.shift();
  });

  test("non-JSON proxy errors do not reveal their body", async () => {
    respond(Buffer.from("<html>private diagnostics</html>"), 502, { "x-request-id": "req-proxy" });
    await assert.rejects(client().listTasks(), error => {
      assert.ok(error instanceof APIError);
      assert.equal(error.requestId, "req-proxy");
      assert.equal(error.statusCode, 502);
      assert.ok(!error.message.includes("private diagnostics"));
      return true;
    });
    requests.shift();
  });

  test("redirects never forward bearer credentials", async () => {
    respond({}, 302, { location: baseUrl + "/credential-leak" });
    await assert.rejects(client().getTask("task-1"), TransportError);
    assert.equal(requests.shift().path, "/api/v1/tasks/task-1");
  });

  test("lost mutation response is reported without resubmission", async () => {
    responses.push((req) => req.socket.destroy());
    await assert.rejects(client().createTask({ goal: "Goal", agentId: "agent-1", budgetMicrousd: 1000 },
                                            { idempotencyKey: "lost-1" }), TransportError);
    assert.equal(requests.shift().headers["idempotency-key"], "lost-1");
  });

  test("timeout covers a response body that never completes", async () => {
    responses.push((_req, res) => {
      res.writeHead(200, { "content-type": "application/json" });
      res.write('{"data":');
    });
    await assert.rejects(client({ timeoutMs: 100 }).getTask("task-1"), error => {
      assert.ok(error instanceof TransportError);
      assert.equal(error.reason, "timeout");
      return true;
    });
    requests.shift();
  });

  test("caller cancellation aborts the request but sends no task cancellation", async () => {
    const controller = new AbortController();
    responses.push(() => controller.abort("caller stopped waiting"));
    await assert.rejects(client().getTask("task-1", { signal: controller.signal }), error => {
      assert.ok(error instanceof TransportError);
      assert.equal(error.reason, "aborted");
      return true;
    });
    assert.equal(requests.shift().method, "GET");
  });

  test("already aborted request does not call token provider or server", async () => {
    const controller = new AbortController();
    controller.abort();
    let called = false;
    await assert.rejects(client({ token: () => { called = true; return "token"; } })
      .listTasks({ signal: controller.signal }), TransportError);
    assert.equal(called, false);
  });

  test("timeout also bounds a hanging token callback", async () => {
    await assert.rejects(client({ timeoutMs: 50, token: () => new Promise(() => {}) }).listTasks(), error => {
      assert.ok(error instanceof TransportError);
      assert.equal(error.reason, "timeout");
      return true;
    });
  });

  test("cancellation racing a rejected token callback is handled", async () => {
    const controller = new AbortController();
    await assert.rejects(client({ token: () => {
      controller.abort();
      return Promise.reject(new Error("token acquisition stopped"));
    } }).listTasks({ signal: controller.signal }), TransportError);
  });

  test("invalid envelopes and unsafe response integers are rejected", async () => {
    const invalid = [Buffer.from("not json"), [], { items: [] }, { data: {} }, { data: [1] },
      { data: [], next_cursor: 4 }, Buffer.from('{"data":[{"budget_microusd":9007199254740993}]}')];
    for (const value of invalid) {
      respond(value);
      await assert.rejects(client().listTasks(), ProtocolError);
      requests.shift();
    }
  });

  test("API root is normalized without duplicate suffix", async () => {
    respond({ data: [] });
    await client({ baseUrl: baseUrl + "/api/v1/" }).listAgents();
    assert.equal(requests.shift().path, "/api/v1/agents");
  });

  test("invalid inputs fail before sending HTTP", async () => {
    const sdk = client();
    const input = { goal: "Goal", agentId: "agent-1", budgetMicrousd: 1 };
    const operations = [
      () => sdk.createTask({ ...input, budgetMicrousd: 1.2 }, { idempotencyKey: "k" }),
      () => sdk.createTask({ ...input, budgetMicrousd: 0 }, { idempotencyKey: "k" }),
      () => sdk.createTask({ ...input, budgetMicrousd: 1_000_000_000_001 }, { idempotencyKey: "k" }),
      () => sdk.createTask({ ...input, budgetMicrousd: Number.MAX_SAFE_INTEGER + 1 }, { idempotencyKey: "k" }),
      () => sdk.createTask({ ...input, budgetMicrousd: true }, { idempotencyKey: "k" }),
      () => sdk.createTask(input, { idempotencyKey: "" }),
      () => sdk.createTask(input, { idempotencyKey: "x".repeat(129) }),
      () => sdk.createTask(input, { idempotencyKey: "key with spaces" }),
      () => sdk.publishAgent("a", { expectedVersion: 0, idempotencyKey: "k" }),
      () => sdk.listTasks({ limit: 101 }),
      () => sdk.runs("t", { limit: 0 }),
      () => sdk.operations("t", { limit: true }),
      () => sdk.actions("t", { limit: 101 }),
      () => sdk.approvals("t", { limit: 1.5 }),
      () => sdk.costs("t", { limit: 101 }),
      () => sdk.costs("../secrets"),
      () => sdk.events("t", { after: 8 }),
      () => sdk.getTask("../secrets"),
    ];
    for (const operation of operations) await assert.rejects(operation);
    assert.throws(() => client({ token: "token\r\nX-Other: value" }));
    assert.throws(() => client({ baseUrl: "https://user:password@example.com" }));
    assert.throws(() => client({ timeoutMs: Infinity }));
  });
});
