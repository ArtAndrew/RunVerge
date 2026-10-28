#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Run the actual Workspace/TLS API/PG OIDC fixture without exposing credentials."""
import os
from pathlib import Path
import subprocess
import sys
import time
import uuid

root = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(root / 'scripts'))
import dev

environment = dev.environment()
environment['TEST_DATABASE_URL'] = environment.get('TEST_DATABASE_URL') or environment.get('DATABASE_URL', '')
if not environment['TEST_DATABASE_URL']:
    raise SystemExit('Local test PostgreSQL is required')
metadata = root / 'web' / '.qa' / ('oidc-smoke-' + uuid.uuid4().hex + '.json')
metadata.parent.mkdir(exist_ok=True)
environment['BROWSER_AUTH_SMOKE_METADATA'] = str(metadata)
fixture = subprocess.Popen(['go', 'test', '-mod=readonly', '-race', '-count=1', './internal/httpapi', '-run', 'TestBrowserLoginWiredAPIAndPermissions', '-v'], cwd=root, env=environment, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
result = 1
try:
    deadline = time.monotonic() + 45
    while not metadata.exists():
        if fixture.poll() is not None or time.monotonic() > deadline:
            raise RuntimeError('TLS fixture did not become ready')
        time.sleep(.1)
    node = environment.get('WORKSPACE_NODE') or 'node'
    result = subprocess.run([node, 'scripts/smoke-oidc.mjs'], cwd=root / 'web', env=environment, timeout=100).returncode
finally:
    if fixture.poll() is None:
        Path(str(metadata) + '.done').write_text('finished\n')
    try:
        output, _ = fixture.communicate(timeout=15)
    except subprocess.TimeoutExpired:
        fixture.terminate()
        output, _ = fixture.communicate(timeout=10)
    sys.stdout.write(output)
    metadata.unlink(missing_ok=True)
    Path(str(metadata) + '.done').unlink(missing_ok=True)
sys.exit(result or fixture.returncode)
