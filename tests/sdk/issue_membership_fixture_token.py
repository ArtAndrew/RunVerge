# SPDX-License-Identifier: Elastic-2.0
"""Trusted-host fixture credential issuance; writes private file, never stdout."""
from __future__ import annotations
import argparse
import importlib.util
from pathlib import Path
import subprocess

ROOT=Path(__file__).resolve().parents[2]
def issue_token(organization_id:str,principal_id:str,destination:Path)->None:
    destination=destination.resolve()
    if ROOT/'.local' not in destination.parents or destination.exists():
        raise ValueError('Use a new private fixture path under this repository .local')
    destination.parent.mkdir(parents=True,exist_ok=True,mode=0o700)
    destination.parent.chmod(0o700)
    spec=importlib.util.spec_from_file_location('membership_fixture_dev_environment',ROOT/'scripts/dev.py')
    dev=importlib.util.module_from_spec(spec);spec.loader.exec_module(dev)
    environment=dev.environment()
    result=subprocess.run([str(ROOT/'bin/platform'),'issue-token','--organization',organization_id,'--principal',principal_id,'--out',str(destination),'--ttl','1h'],cwd=ROOT,env=environment,capture_output=True,text=True,timeout=20)
    if result.returncode:raise RuntimeError('Trusted fixture credential issuance failed; inspect locally without printing secrets')
    if destination.is_symlink() or not destination.is_file() or destination.stat().st_mode&0o077:
        raise RuntimeError('Trusted fixture issuer did not create a private regular credential file')

if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--organization',required=True);parser.add_argument('--principal',required=True);parser.add_argument('--out',type=Path,required=True);args=parser.parse_args()
    issue_token(args.organization,args.principal,args.out)
