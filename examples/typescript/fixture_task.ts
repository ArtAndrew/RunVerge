// SPDX-License-Identifier: Apache-2.0
/** Run with Node 24 after building packages/typescript-sdk. Explicit fixture only. */
import { randomUUID } from "node:crypto";
import { parseArgs } from "node:util";
import { setTimeout as delay } from "node:timers/promises";
import { AgentPlatformError, Client } from "../../packages/typescript-sdk/dist/index.js";

const { values } = parseArgs({
  options: {
    help: { type: "boolean", short: "h", default: false },
    url: { type: "string", default: process.env.AGENT_PLATFORM_URL ?? "http://127.0.0.1:8080" },
    "request-key": { type: "string" },
    "wait-seconds": { type: "string", default: "30" },
  },
});

if (values.help) {
  console.log(`Usage: node examples/typescript/fixture_task.ts [options]

Creates and publishes an explicit fixture:echo agent, submits a task,
polls stored events, and prints text artifacts. This does not run a real LLM.

Environment:
  AGENT_PLATFORM_TOKEN                Authorized platform token
  AGENT_PLATFORM_ORGANIZATION_ID      Existing organization membership
  AGENT_PLATFORM_URL                  Optional server URL

Options:
  --url URL                 Server origin (default http://127.0.0.1:8080)
  --request-key KEY         Stable key; reuse after a lost response
  --wait-seconds SECONDS    Maximum polling wait (default 30)
  --help                    Show this help`);
  process.exit(0);
}

function requiredEnv(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`Set ${name}`);
  return value;
}

async function main(): Promise<number> {
  const waitSeconds = Number(values["wait-seconds"]);
  if (!Number.isFinite(waitSeconds) || waitSeconds <= 0) {
    throw new Error("--wait-seconds must be finite and positive");
  }
  const requestKey = values["request-key"] ?? randomUUID();
  if (!/^[\x21-\x7E]{1,100}$/.test(requestKey)) {
    throw new Error("--request-key must be 1–100 printable ASCII characters without spaces");
  }
  const client = new Client({
    baseUrl: values.url,
    token: requiredEnv("AGENT_PLATFORM_TOKEN"),
    organizationId: requiredEnv("AGENT_PLATFORM_ORGANIZATION_ID"),
  });
  console.log(`Development fixture, not a real model. Request key: ${requestKey}`);
  try {
    const draft = await client.createAgent({
      name: "TypeScript SDK fixture example",
      instructions: "Execute the explicit echo fixture and save its artifact.",
      model: "fixture:echo", tools: [], budgetMicrousd: 10_000,
    }, { idempotencyKey: requestKey + ":agent" });
    const agent = await client.publishAgent(draft.id, {
      expectedVersion: draft.version, idempotencyKey: requestKey + ":publish",
    });
    let task = await client.createTask({
      goal: "Verify the TypeScript SDK against the platform's development fixture.",
      agentId: agent.id, budgetMicrousd: 10_000,
    }, { idempotencyKey: requestKey + ":task" });
    console.log(`Task: ${task.id}`);
    const deadline = performance.now() + waitSeconds * 1000;
    // Preserve the wire string: Number(sequence) would round values above 2^53 - 1.
    let after = "0";
    while (true) {
      const page = await client.events(task.id, { after, limit: 100 });
      for (const event of page.data) {
        console.log(`${event.sequence}: ${event.type}`);
        after = event.sequence;
      }
      task = await client.getTask(task.id);
      if (["succeeded", "failed", "cancelled"].includes(task.status)) {
        console.log(`Final status: ${task.status}`);
        for (const artifact of (await client.artifacts(task.id)).data) {
          console.log(`\n${artifact.name} (${artifact.media_type}):\n${artifact.content}`);
        }
        return task.status === "succeeded" ? 0 : 1;
      }
      if (performance.now() >= deadline) {
        console.error(`Wait expired; task ${task.id} remains ${task.status}. It was not cancelled.`);
        return 2;
      }
      await delay(Math.min(500, Math.max(0, deadline - performance.now())));
    }
  } catch (error) {
    if (!(error instanceof AgentPlatformError)) throw error;
    console.error(`${error.message}\nRetry with the same --request-key ${JSON.stringify(requestKey)} if the result is unknown.`);
    return 1;
  }
}

try {
  process.exitCode = await main();
} catch (error) {
  console.error(error instanceof Error ? error.message : "Example failed");
  process.exitCode = 1;
}
