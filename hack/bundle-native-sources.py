#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Archive pinned BusyBox/uClibc source and the upstream build recipes.

The output is a persistent release asset, separate from short-lived CI artifacts.
Every downloaded or cached input must match the reviewed SHA-256 lock.
"""
import argparse
import gzip
import hashlib
import io
import json
from pathlib import Path, PurePosixPath
import tarfile
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
LOCK = ROOT / 'build/licenses/busybox/sources.lock.json'


def checked_bytes(resource, cache=None):
    name = PurePosixPath(resource['name'])
    if name.is_absolute() or '..' in name.parts:
        raise ValueError(f'Unsafe source path: {name}')
    cached = cache / str(name) if cache else None
    if cached and cached.exists():
        data = cached.read_bytes()
    else:
        if not resource['url'].startswith('https://'):
            raise ValueError('Source downloads require HTTPS')
        with urllib.request.urlopen(resource['url'], timeout=120) as response:
            data = response.read()
    if hashlib.sha256(data).hexdigest() != resource['sha256']:
        raise ValueError(f'Source checksum mismatch: {name}')
    return data


def bundle(output, cache=None):
    lock = json.loads(LOCK.read_text())
    if f"FROM {lock['image']} AS busybox" not in (ROOT / 'build/Dockerfile').read_text():
        raise ValueError('BusyBox Docker image differs from the source lock')
    entries = {item['name']: checked_bytes(item, cache) for item in lock['resources']}
    if len(entries) != len(lock['resources']):
        raise ValueError('Duplicate source archive paths')
    entries['sources.lock.json'] = LOCK.read_bytes()
    for name in ('LICENSE', 'OFFER', 'UCLIBC-LICENSE'):
        entries[name] = (LOCK.parent / name).read_bytes()
    output.parent.mkdir(parents=True, exist_ok=True)
    # Stable metadata lets maintainers compare independently downloaded bundles.
    with output.open('wb') as raw, gzip.GzipFile(filename='', mode='wb', fileobj=raw, mtime=0) as zipped:
        with tarfile.open(fileobj=zipped, mode='w') as archive:
            for name, data in sorted(entries.items()):
                info = tarfile.TarInfo(name)
                info.size, info.mode, info.mtime = len(data), 0o644, 0
                archive.addfile(info, io.BytesIO(data))
    print(f'Native source archive: {output} ({len(entries)} files)')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', required=True, type=Path)
    parser.add_argument('--cache', type=Path, help='Optional input directory; checksums still required')
    args = parser.parse_args()
    bundle(args.output, args.cache)
