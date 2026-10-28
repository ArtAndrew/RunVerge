"""Run the public examples against bounded HTTP fixtures, without paid calls."""

from __future__ import annotations

import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import threading
import unittest

from test_client import ContractServer


ROOT = Path(__file__).resolve().parents[2]
ORG, TASK = "org-example", "task-example"
TOKEN = "synthetic-example-private-token"
SEQUENCES = ("9007199254740993", "9223372036854775807")


class PublicExampleTests(unittest.TestCase):
    def run_example(self, command: list[str]) -> None:
        server = ContractServer()
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        task = dict(id=TASK, organization_id=ORG, status="queued",
                    reserved_microusd="0", spent_microusd="0")
        responses = [
            (201, {"data": dict(id="agent-example", version=1)}, {}),
            (200, {"data": dict(id="agent-example", version=1, published=True)}, {}),
            (201, {"data": task}, {}),
        ]
        for index, sequence in enumerate(SEQUENCES):
            event = dict(id="event-" + sequence, organization_id=ORG, task_id=TASK,
                         sequence=sequence, type="historical.example", payload_version=0,
                         legacy_data_json='{"wide":9007199254740993}',
                         created_at="2026-10-25T00:00:00Z")
            responses.extend([
                (200, {"data": [event]}, {}),
                (200, {"data": {**task, "status": "queued" if index == 0 else "succeeded"}}, {}),
            ])
        responses.append((200, {"data": [dict(name="Fixture result", media_type="text/plain",
                                             content="Synthetic fixture completed.")]}, {}))
        for response in responses:
            server.responses.put(response)
        env = os.environ.copy()
        env.update(AGENT_PLATFORM_URL=f"http://127.0.0.1:{server.server_port}",
                   AGENT_PLATFORM_TOKEN=TOKEN, AGENT_PLATFORM_ORGANIZATION_ID=ORG,
                   PYTHONPATH=str(ROOT / "packages/python-sdk/src"), NO_PROXY="127.0.0.1")
        try:
            # An unrelated working directory also proves imports do not depend on cwd.
            with tempfile.TemporaryDirectory(prefix="platform-example-") as cwd:
                result = subprocess.run([*command, "--request-key", "exact-example",
                                         "--wait-seconds", "5"], cwd=cwd, env=env,
                                        capture_output=True, text=True, timeout=15)
            output = result.stdout + result.stderr
            self.assertNotIn(TOKEN, output, "Example exposed its credential")
            self.assertEqual(result.returncode, 0, output)
            self.assertIn("Final status: succeeded", output)
            self.assertIn("Synthetic fixture completed.", output)
            for sequence in SEQUENCES:
                self.assertEqual(result.stdout.count(sequence + ": historical.example"), 1)
            self.assertTrue(server.responses.empty(), "Example did not consume the expected flow")
            requests = [server.requests.get_nowait() for _ in responses]
            self.assertTrue(server.requests.empty(), "Unexpected extra command or read")
            self.assertEqual([(request["method"], request["path"]) for request in requests], [
                ("POST", "/api/v1/agents"),
                ("POST", "/api/v1/agents/agent-example/publish"),
                ("POST", "/api/v1/tasks"),
                ("GET", f"/api/v1/tasks/{TASK}/events?after=0&limit=100"),
                ("GET", f"/api/v1/tasks/{TASK}"),
                ("GET", f"/api/v1/tasks/{TASK}/events?after={SEQUENCES[0]}&limit=100"),
                ("GET", f"/api/v1/tasks/{TASK}"),
                ("GET", f"/api/v1/tasks/{TASK}/artifacts"),
            ])
            for request in requests:
                self.assertEqual(request["headers"]["Authorization"], "Bearer " + TOKEN)
                self.assertEqual(request["headers"]["X-Organization-ID"], ORG)
                self.assertEqual(request["headers"]["X-Platform-API-Version"], "0.14.0")
            self.assertEqual([request["headers"].get("Idempotency-Key") for request in requests],
                             ["exact-example:agent", "exact-example:publish", "exact-example:task",
                              None, None, None, None, None])
            self.assertEqual(requests[0]["body"]["model"], "fixture:echo")
            self.assertEqual(requests[0]["body"]["tools"], [])
            self.assertEqual(requests[1]["body"], {"expected_version": 1})
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=2)

    def test_python_example_preserves_full_int64_event_sequences(self) -> None:
        self.run_example([sys.executable, str(ROOT / "examples/python/fixture_task.py")])

    def test_typescript_example_preserves_full_int64_event_sequences(self) -> None:
        node = os.environ.get("NODE_BINARY") or shutil.which("node")
        self.assertIsNotNone(node, "Node.js 24 is required to test the public TypeScript example")
        self.run_example([node, str(ROOT / "examples/typescript/fixture_task.ts")])


if __name__ == "__main__":
    unittest.main()
