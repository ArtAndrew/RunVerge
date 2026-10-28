#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Reproduce foundation checks. This is NOT the full-product release gate."""
import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys

import dev

ROOT = dev.ROOT
EVIDENCE = ROOT/'docs/evidence'
VERSION = re.search(r'const ContractVersion = "([0-9.]+)"', (ROOT/'internal/domain/types.go').read_text()).group(1)
REPORT_NAME = 'core-' + VERSION

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--report-name', default=REPORT_NAME, help='Evidence filename prefix; use a new name to preserve earlier verification')
    args=parser.parse_args()
    if not re.fullmatch(r'[a-zA-Z0-9][a-zA-Z0-9_.-]{0,79}', args.report_name):
        parser.error('Report name must be a bounded filename prefix without directories')
    report_name=args.report_name
    EVIDENCE.mkdir(parents=True, exist_ok=True)
    env=dev.environment()
    if not env.get('DATABASE_URL'):
        raise SystemExit('A real local PostgreSQL database is required')
    env['TEST_DATABASE_URL']=env['DATABASE_URL']
    node=dev.executable('node')
    env['PATH']=str(Path(node).parent)+os.pathsep+env['PATH']
    python=shutil_python()
    checks=[
        ('go-vet',['go','vet','./...'],ROOT),
        ('go-race',['go','test','-race','-count=1','-json','./...'],ROOT),
        ('contracts',[python,'tests/contracts/test_contracts.py'],ROOT),
        ('python-sdk',[python,'-m','unittest','discover','-s','tests/sdk','-p','test_*.py','-v'],ROOT),
        ('development-tools',[python,'-m','unittest','discover','-s','tests/development','-p','test_*.py','-v'],ROOT),
        ('synthetic-peers',[python,'-m','unittest','discover','-s','tests/fixtures','-p','test_*.py','-v'],ROOT),
        ('typescript-sdk',[node,'node_modules/typescript/bin/tsc','-p','tsconfig.json'],ROOT/'packages/typescript-sdk'),
        ('typescript-sdk-http',[node,'--test',*map(str,(ROOT/'packages/typescript-sdk/tests').glob('*.test.mjs'))],ROOT/'packages/typescript-sdk'),
        ('web-typecheck',[node,'node_modules/typescript/bin/tsc','--noEmit'],ROOT/'web'),
        ('web-build',[node,'node_modules/vite/bin/vite.js','build'],ROOT/'web'),
        ('web-unit',[node,'--test',*map(str,sorted((ROOT/'web/tests').glob('*.test.mjs')))],ROOT/'web'),
    ]
    initial_manifest=source_manifest()
    results=[]
    for name,args,cwd in checks:
        started=datetime.datetime.now(datetime.timezone.utc)
        process=subprocess.run(args,cwd=cwd,env=env,capture_output=True,text=True)
        log=EVIDENCE/(report_name+'-'+name+'.log')
        output=process.stdout+process.stderr
        # Credential material can only enter through environment, never args.
        # Still redact the configured DSN from any unexpected tool output.
        output=output.replace(env['DATABASE_URL'],'[REDACTED_DATABASE_URL]')
        log.write_text(output)
        results.append({'name':name,'command':args,'cwd':str(cwd.relative_to(ROOT)),'exit_code':process.returncode,'passed':process.returncode==0,'started_at':started.isoformat(),'duration_seconds':(datetime.datetime.now(datetime.timezone.utc)-started).total_seconds(),'log':str(log.relative_to(ROOT))})
        print(name+': '+('PASS' if process.returncode==0 else 'FAIL'))
    tracked=source_manifest()
    stable=initial_manifest==tracked
    report={'scope':'current_core_slice_not_full_product','contract_version':VERSION,'created_at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'go_toolchain':subprocess.check_output(['go','version'],env=env,text=True).strip(),'node':subprocess.check_output([node,'--version'],text=True).strip(),'source_files':tracked,'source_stable_during_checks':stable,'checks':results,'passed':stable and all(r['passed'] for r in results),'not_proven':['named_paid_provider_interoperability_and_final_billing','external_action_reconciliation','complete_corporate_SSO_lifecycle_and_enforcement','full_MCP_OAuth_and_external_peer_acceptance','sandbox','complete_multiagent_quality_recovery_and_action_workflows','provider_lifecycle','full_performance_profile','independent_role_usability','backup_restore_release']}
    if not stable:
        print('source stability: FAIL; inputs changed during verification')
    (EVIDENCE/(report_name+'-report.json')).write_text(json.dumps(report,indent=2)+'\n')
    if not report['passed']:
        raise SystemExit(1)

def source_manifest():
    result={}
    for directory in ['cmd','internal','contracts','packages','web/src','web/tests','web/scripts','examples','scripts','tests']:
        for p in sorted((ROOT/directory).rglob('*')):
            if not p.is_file() or any(part in ['node_modules','dist','__pycache__','build','.deps','.scratch','.local'] or part.endswith('.egg-info') for part in p.parts):
                continue
            if p.suffix not in ['.go','.sql','.json','.py','.ts','.tsx','.mjs','.css','.toml']:
                continue
            if directory=='tests' and 'report' in p.name:
                continue # Generated evidence is output, not a test input.
            result[str(p.relative_to(ROOT))]=hashlib.sha256(p.read_bytes()).hexdigest()
    for name in ['go.mod','go.sum','web/package.json','web/pnpm-lock.yaml','web/tsconfig.json','web/vite.config.ts','web/index.html','packages/typescript-sdk/package.json','packages/typescript-sdk/pnpm-lock.yaml']:
        p=ROOT/name
        if p.exists(): result[name]=hashlib.sha256(p.read_bytes()).hexdigest()
    return result

def shutil_python():
    if sys.version_info>=(3,10): return sys.executable
    bundled=Path.home()/'.cache/codex-runtimes/codex-primary-runtime/dependencies/python/bin/python3'
    if bundled.exists():return str(bundled)
    raise SystemExit('Python >=3.10 is required for the Python SDK checks')

if __name__=='__main__':
    main()
