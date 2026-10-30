#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Own the local model process for the opt-in Linux container pilot.

Run inside the disposable model-pilot-runtime image. The workflow verifies the
official engine archive checksum before mounting it read-only. No host keys,
database configuration or models directory are mounted.
"""
import argparse
import contextlib
import hashlib
import json
from pathlib import Path
import subprocess
import shutil
import tempfile
import time
from urllib.request import build_opener, ProxyHandler

from verify_native_bundle import verify, unused_port, stop_owned_child
from verify_team_model_mcp import TeamModelMCPProbe, MODEL

ENGINE_VERSION = '0.40.11'
MODEL_DIGEST = '65ec06548149b04c096a120e4a6da9d4017ea809c91734ea5631e89f96ddc57b'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--ollama', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    output = args.output.absolute()
    output.mkdir(mode=0o700, parents=False, exist_ok=False)
    engine_report = {'passed': False, 'engine_version': ENGINE_VERSION, 'model': MODEL,
                     'model_manifest_sha256': MODEL_DIGEST, 'cloud_disabled': True,
                     'paid_api_used': False, 'ollama_stopped': False}
    child = None
    # Do not remove a possibly live engine's model directory on cleanup failure.
    # The enclosing workflow also stops/removes this owned container.
    with contextlib.nullcontext(tempfile.mkdtemp(prefix='runverge-team-model-')) as private:
        private = Path(private)
        env = {'PATH': '/usr/bin:/bin', 'HOME': str(private), 'TMPDIR': str(private), 'LANG': 'C',
               'OLLAMA_HOST': '127.0.0.1:%d' % unused_port(), 'OLLAMA_MODELS': str(private / 'models'),
               'OLLAMA_NO_CLOUD': '1', 'OLLAMA_NUM_PARALLEL': '1', 'OLLAMA_MAX_LOADED_MODELS': '1',
               'OLLAMA_CONTEXT_LENGTH': '8192', 'OLLAMA_KEEP_ALIVE': '5m'}
        base = 'http://' + env['OLLAMA_HOST']
        opener = build_opener(ProxyHandler({}))
        def get(route):
            with opener.open(base + route, timeout=3) as response:
                raw = response.read(1024 * 1024 + 1)
            if len(raw) > 1024 * 1024:
                raise RuntimeError('Owned model metadata exceeded bound')
            return json.loads(raw)
        with (private / 'engine.log').open('x') as log:
            try:
                child = subprocess.Popen([str(args.ollama), 'serve'], env=env, stdout=log, stderr=log)
                deadline = time.monotonic() + 30
                while True:
                    if child.poll() is not None or time.monotonic() >= deadline:
                        raise RuntimeError('Owned model engine startup failed')
                    try:
                        version = get('/api/version')
                        break
                    except (OSError, ValueError):
                        time.sleep(0.1)
                if version.get('version') != ENGINE_VERSION:
                    raise RuntimeError('Owned model engine version mismatch')
                print('Pinned local model engine ready; downloading the public model.', flush=True)
                pull = subprocess.run([str(args.ollama), 'pull', MODEL], env=env,
                                      stdout=log, stderr=log, timeout=900)
                if pull.returncode:
                    raise RuntimeError('Owned model download failed; no inference attempted')
                models = [v for v in get('/api/tags')['models'] if v['name'] == MODEL]
                if len(models) != 1 or models[0]['digest'].removeprefix('sha256:') != MODEL_DIGEST:
                    raise RuntimeError('Model manifest differs from the reviewed pin; no inference attempted')
                engine_report['model_bytes'] = models[0]['size']
                print('Model manifest verified; running bounded team and MCP workflow.', flush=True)
                bundle = Path('/opt/bundle')
                probe = TeamModelMCPProbe(base + '/v1/chat/completions', Path('/opt/test-tools/mcp-read-server'),
                                          output / 'synthetic-model-samples.json')
                report = verify(bundle, hashlib.sha256((bundle / 'bundle.json').read_bytes()).hexdigest(),
                                output / 'team-model-native.json', extra_probe=probe)
                engine_report['passed'] = report['passed'] and report['additional_probe'].get('restart_verified') is True
            finally:
                try:
                    if child is not None:
                        stop_owned_child(child)
                        engine_report['ollama_stopped'] = child.poll() is not None
                finally:
                    if not engine_report['ollama_stopped']:
                        engine_report['passed'] = False
                    (output / 'engine.json').write_text(json.dumps(engine_report, indent=2) + '\n')
                    if child is None or engine_report['ollama_stopped']:
                        shutil.rmtree(private)
    if not engine_report['passed'] or not engine_report['ollama_stopped']:
        raise RuntimeError('Team model pilot incomplete')
    print('Team model and MCP pilot passed; synthetic output and bounded evidence saved.', flush=True)


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        # Fixed failures only; never print a command environment, provider body,
        # credential file or private process log into public CI output.
        raise SystemExit(str(error) if isinstance(error, RuntimeError) else
                         'Team model pilot failed: ' + type(error).__name__) from None
