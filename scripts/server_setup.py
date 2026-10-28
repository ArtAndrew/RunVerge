#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Create new private Compose settings. Does not start, upgrade or delete data."""
import argparse
import json
import os
from pathlib import Path
import secrets
import stat
import sys
from urllib.parse import quote


def initialize(directory):
    directory=Path(os.path.abspath(directory))
    parent=directory.parent.lstat()
    if not stat.S_ISDIR(parent.st_mode) or parent.st_mode & 0o022 or parent.st_uid!=os.getuid():
        raise ValueError('Choose an existing parent directory owned by you and not writable by others')
    directory.mkdir(mode=0o700)
    password=secrets.token_urlsafe(32)
    values={
        'db.env':'POSTGRES_PASSWORD='+password+'\n',
        'runtime.env':'DATABASE_URL=postgres://runverge:'+quote(password,safe='')+'@db:5432/runverge?sslmode=disable\n',
        'model.env':'# Add only explicitly selected model credentials here.\n',
    }
    for name,body in values.items():
        fd=os.open(directory/name,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
        with os.fdopen(fd,'w') as stream:
            os.fchmod(stream.fileno(),0o600)
            stream.write(body)
            stream.flush();os.fsync(stream.fileno())
    return {'created':True,'private_directory':str(directory),'files':list(values),'services_started':False}


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--directory',type=Path,default=Path(__file__).resolve().parents[1]/'deploy/private')
    args=parser.parse_args()
    try:
        print(json.dumps(initialize(args.directory),indent=2))
    except (ValueError,OSError) as error:
        print(str(error) if isinstance(error,ValueError) else 'Private settings were not completed; existing files are never replaced. Inspect the selected directory.',file=sys.stderr)
        return 1
    return 0


if __name__=='__main__':
    raise SystemExit(main())
