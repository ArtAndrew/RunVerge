#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Create new private Compose settings. Does not start, upgrade or delete data."""
import argparse
import base64
import hashlib
import hmac
import json
import os
from pathlib import Path
import secrets
import stat
import sys
from urllib.parse import quote


def scram_verifier(password):
    """PostgreSQL SCRAM-SHA-256 verifier for a freshly generated ASCII secret."""
    salt = secrets.token_bytes(16)
    salted = hashlib.pbkdf2_hmac('sha256', password.encode('ascii'), salt, 4096)
    client_key = hmac.digest(salted, b'Client Key', 'sha256')
    stored_key = hashlib.sha256(client_key).digest()
    server_key = hmac.digest(salted, b'Server Key', 'sha256')
    encode = lambda raw: base64.b64encode(raw).decode('ascii')
    return 'SCRAM-SHA-256$4096:' + encode(salt) + '$' + encode(stored_key) + ':' + encode(server_key)


def initialize(directory):
    directory=Path(os.path.abspath(directory))
    parent=directory.parent.lstat()
    if not stat.S_ISDIR(parent.st_mode) or parent.st_mode & 0o022 or parent.st_uid!=os.getuid():
        raise ValueError('Choose an existing parent directory owned by you and not writable by others')
    directory.mkdir(mode=0o700)
    admin_password=secrets.token_urlsafe(32)
    runtime_password=secrets.token_urlsafe(32)
    profile=secrets.token_hex(16)
    values={
        # Compose single quotes preserve the verifier's literal dollar signs.
        # Only the DB container receives the bootstrap secret and verifier.
        'db.env': 'POSTGRES_PASSWORD='+admin_password+'\n'
                  +"RUNVERGE_APP_VERIFIER='"+scram_verifier(runtime_password)+"'\n"
                  +'RUNVERGE_CREDENTIAL_PROFILE='+profile+'\n',
        'runtime.env':'DATABASE_URL=postgres://runverge:'+quote(runtime_password,safe='')+'@db:5432/runverge?sslmode=disable\n',
        'model.env':'# Add only explicitly selected model credentials here.\n',
    }
    for name,body in values.items():
        fd=os.open(directory/name,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
        with os.fdopen(fd,'w') as stream:
            os.fchmod(stream.fileno(),0o600)
            stream.write(body)
            stream.flush();os.fsync(stream.fileno())
    return {'created':True,'private_directory':str(directory),'files':list(values),'services_started':False,
            'database_profile':'separate_admin_and_application_owner_v1'}


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
