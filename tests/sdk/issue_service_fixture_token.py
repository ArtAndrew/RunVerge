# SPDX-License-Identifier: Elastic-2.0
"""Trusted local fixture issuer: secret only in a new private file, never stdout."""
from __future__ import annotations
import argparse
import importlib.util
from pathlib import Path
import subprocess
ROOT=Path(__file__).resolve().parents[2]
def issue_service_token(organization_id:str,principal_id:str,destination:Path,role_ceiling:str='viewer',ttl_seconds:int=60)->None:
    destination=destination.resolve()
    if ROOT/'.local' not in destination.parents or destination.exists() or role_ceiling not in ('member','viewer') or type(ttl_seconds) is not int or not 1<=ttl_seconds<=86400:
        raise ValueError('Use a new private fixture file and bounded service role/TTL')
    destination.parent.mkdir(parents=True,exist_ok=True,mode=0o700);destination.parent.chmod(0o700)
    spec=importlib.util.spec_from_file_location('security_fixture_dev_environment',ROOT/'scripts/dev.py');dev=importlib.util.module_from_spec(spec);spec.loader.exec_module(dev)
    result=subprocess.run([str(ROOT/'bin/platform'),'issue-service-token','--organization',organization_id,'--principal',principal_id,'--role-ceiling',role_ceiling,'--ttl',str(ttl_seconds)+'s','--reason','Synthetic local SDK/browser service-key verification','--out',str(destination)],cwd=ROOT,env=dev.environment(),capture_output=True,text=True,timeout=20)
    if result.returncode:raise RuntimeError('Trusted service fixture issuance failed; output withheld')
    if destination.is_symlink() or not destination.is_file() or destination.stat().st_mode&0o077:raise RuntimeError('Fixture issuer did not create a private regular file')
if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--organization',required=True);parser.add_argument('--principal',required=True);parser.add_argument('--out',type=Path,required=True);parser.add_argument('--role-ceiling',choices=['member','viewer'],default='viewer');parser.add_argument('--ttl-seconds',type=int,default=60);args=parser.parse_args();issue_service_token(args.organization,args.principal,args.out,args.role_ceiling,args.ttl_seconds)
