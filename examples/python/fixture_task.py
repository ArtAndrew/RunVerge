# SPDX-License-Identifier: Apache-2.0
"""Exercise a published fixture agent through the real HTTP API.

This intentionally uses fixture:echo. It verifies API integration, not LLM
quality. Install packages/python-sdk or set PYTHONPATH before running.
"""

from __future__ import annotations

import argparse
import math
import os
import sys
import time
import uuid

from agent_platform import AgentPlatformError, Client


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default=os.getenv("AGENT_PLATFORM_URL", "http://127.0.0.1:8080"))
    parser.add_argument("--request-key", default=None,
                        help="Stable operation key; reuse after a lost response (maximum 100 ASCII characters)")
    parser.add_argument("--wait-seconds", type=float, default=30.0)
    args = parser.parse_args()
    token = os.getenv("AGENT_PLATFORM_TOKEN")
    organization = os.getenv("AGENT_PLATFORM_ORGANIZATION_ID")
    if not token or not organization:
        parser.error("Set AGENT_PLATFORM_TOKEN and AGENT_PLATFORM_ORGANIZATION_ID")
    if not math.isfinite(args.wait_seconds) or args.wait_seconds <= 0:
        parser.error("--wait-seconds must be finite and positive")
    request_key = args.request_key or str(uuid.uuid4())
    if not request_key or len(request_key) > 100 or any(ord(char) < 33 or ord(char) > 126 for char in request_key):
        parser.error("--request-key must be 1–100 printable ASCII characters")
    print(f"Development fixture, not a real model. Request key: {request_key}", flush=True)
    client = Client(args.url, token=token, organization_id=organization)

    try:
        draft = client.create_agent(
            "Python SDK fixture example",
            "Execute the explicit echo fixture and save its artifact.",
            model="fixture:echo", tools=[], budget_microusd=10_000,
            idempotency_key=request_key + ":agent",
        )
        agent = client.publish_agent(
            draft["id"], expected_version=draft["version"],
            idempotency_key=request_key + ":publish",
        )
        task = client.create_task(
            "Verify the Python SDK against the platform's development fixture.",
            agent["id"], 10_000,
            idempotency_key=request_key + ":task",
        )
        print(f"Task: {task['id']}", flush=True)
        deadline = time.monotonic() + args.wait_seconds
        # Keep the exact decimal string; sequences can exceed JavaScript's safe integer range.
        last_sequence = "0"
        while True:
            page = client.events(task["id"], after=last_sequence, limit=100)
            for event in page.data:
                print(f"{event['sequence']}: {event['type']}", flush=True)
                last_sequence = event["sequence"]
            task = client.get_task(task["id"])
            if task["status"] in ("succeeded", "failed", "cancelled"):
                print(f"Final status: {task['status']}")
                for artifact in client.artifacts(task["id"]).data:
                    print(f"\n{artifact['name']} ({artifact['media_type']}):\n{artifact['content']}")
                return 0 if task["status"] == "succeeded" else 1
            if time.monotonic() >= deadline:
                print(f"Wait expired; task {task['id']} remains {task['status']}. It was not cancelled.", file=sys.stderr)
                return 2
            time.sleep(min(0.5, max(0.0, deadline - time.monotonic())))
    except AgentPlatformError as error:
        print(f"{error}\nRetry with the same --request-key {request_key!r} if the result is unknown.", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
