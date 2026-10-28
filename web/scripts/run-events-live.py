# SPDX-License-Identifier: Elastic-2.0
"""Opt-in isolated actual TLS API/PG event tests through both SDKs and built UI."""
from __future__ import annotations
import argparse
import importlib.util
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time
import uuid

ROOT=Path(__file__).resolve().parents[2]


def main() -> int:
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--output',type=Path,required=True);args=parser.parse_args()
    output=args.output.resolve()
    if output.exists():parser.error('Use a new output directory; historical evidence is immutable')
    output.mkdir(parents=True,mode=0o700);output.chmod(0o700)
    spec=importlib.util.spec_from_file_location('platform_dev',ROOT/'scripts/dev.py');dev=importlib.util.module_from_spec(spec);spec.loader.exec_module(dev)
    env=dev.environment();env['TEST_DATABASE_URL']=env.get('TEST_DATABASE_URL') or env.get('DATABASE_URL','')
    if not env['TEST_DATABASE_URL']:parser.error('Local test PostgreSQL is required')
    private=ROOT/'.local/qa/event-stream'/uuid.uuid4().hex;private.mkdir(parents=True,mode=0o700);private.chmod(0o700);metadata=private/'fixture.json'
    env.update(PLATFORM_EVENT_SMOKE_METADATA=str(metadata),AGENT_PLATFORM_QA_OUTPUT=str(output))
    fixture_log=output/'event-fixture.log';commands=[];passed=False;phase='fixture_ready';fixture=None;error_type=None
    def run(command,timeout=35):
        commands.append([str(value) for value in command]);result=subprocess.run(command,cwd=ROOT,env=env,text=True,capture_output=True,timeout=timeout)
        (output/('phase-'+str(len(commands))+'.log')).write_text(result.stdout+result.stderr)
        if result.returncode:raise RuntimeError('Event consumer phase failed')
    with fixture_log.open('w') as log:
        fixture=subprocess.Popen(['go','test','-mod=readonly','-race','-count=1','./internal/httpapi','-run','^TestEventConsumerHandoffFixture$','-v','-timeout=210s'],cwd=ROOT,env=env,stdout=log,stderr=subprocess.STDOUT,start_new_session=True)
        try:
            deadline=time.monotonic()+70
            while not metadata.exists():
                if fixture.poll() is not None or time.monotonic()>deadline:raise RuntimeError('Isolated fixture did not become ready')
                time.sleep(.05)
            sys.path.insert(0,str(ROOT/'tests/sdk'));from events_execution import private_json
            facts=private_json(metadata)
            env['SSL_CERT_FILE']=facts['ca_file'];env['NODE_EXTRA_CA_CERTS']=facts['ca_file']
            node=env.get('WORKSPACE_NODE') or 'node'
            phase='python';run([sys.executable,str(ROOT/'tests/sdk/events_execution.py'),str(metadata),str(output/'events-python-live.json')])
            phase='typescript';run([node,str(ROOT/'tests/sdk/events_execution.ts'),str(metadata),str(output/'events-typescript-live.json')])
            phase='browser';run([node,str(ROOT/'web/scripts/smoke-events-live.mjs')],65)
            passed=True
        except Exception as exc:error_type=type(exc).__name__
        finally:
            done=Path(str(metadata)+'.done')
            if not done.exists():
                descriptor=os.open(done,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
                with os.fdopen(descriptor,'w') as target:target.write('finished\n')
            try:fixture.wait(timeout=20)
            except subprocess.TimeoutExpired:
                os.killpg(fixture.pid,signal.SIGTERM)
                try:fixture.wait(timeout=8)
                except subprocess.TimeoutExpired:os.killpg(fixture.pid,signal.SIGKILL);fixture.wait(timeout=5)
    passed=passed and fixture.returncode==0
    report=dict(contract_version='0.13.0',passed=passed,last_phase=phase,error_type=error_type,fixture_exit_code=fixture.returncode,commands=commands,scope='isolated schema and actual TLS API; main services untouched',real_provider_calls=0)
    (output/'events-live-runner-report-0.11.json').write_text(json.dumps(report,indent=2)+'\n');print(json.dumps({key:value for key,value in report.items() if key!='commands'}))
    return 0 if passed else 1


if __name__=='__main__':raise SystemExit(main())
