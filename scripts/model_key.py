#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Save an OpenRouter key to a new private worker env_file, without printing it."""
import argparse
import getpass
import os
from pathlib import Path
import re
import stat
import sys


def load_key(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, 'rb') as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_uid != os.getuid() or not 32 <= info.st_size <= 513:
            raise ValueError('A private owned key file is required')
        value = stream.read(514).decode('ascii').strip()
    if not re.fullmatch(r'[A-Za-z0-9._-]{32,512}', value):
        raise ValueError('Invalid private key format')
    return value


def save_key(path, key):
    if not isinstance(key, str) or not re.fullmatch(r'[A-Za-z0-9._-]{32,512}', key):
        raise ValueError('Key must be one bounded token without whitespace or shell characters')
    path = Path(os.path.abspath(path))
    before = path.parent.lstat()
    if not stat.S_ISDIR(before.st_mode) or before.st_mode & 0o077 or before.st_uid != os.getuid():
        raise ValueError('Choose an existing private directory owned by you')
    parent = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        after = os.fstat(parent)
        if (before.st_dev, before.st_ino) != (after.st_dev, after.st_ino):
            raise ValueError('Private directory changed')
        fd = os.open(path.name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=parent)
        with os.fdopen(fd, 'w') as stream:
            os.fchmod(stream.fileno(), 0o600)
            stream.write('PLATFORM_MODEL_KEY_OPENROUTER=' + key + '\n')
            stream.flush()
            os.fsync(stream.fileno())
    finally:
        os.close(parent)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--key-file', type=Path, help='existing private raw-key file; otherwise use hidden terminal input')
    args = parser.parse_args()
    try:
        if args.key_file is None and not sys.stdin.isatty():
            raise ValueError('Use an interactive terminal for hidden key entry')
        value = load_key(args.key_file) if args.key_file else getpass.getpass('OpenRouter test key (hidden): ')
        save_key(args.out, value)
        print('Private worker key file created. No connection or model request was made.')
    except (ValueError, UnicodeError, OSError):
        raise SystemExit('Key file was not completed. Use a new filename in a private directory; existing files are never replaced.') from None
