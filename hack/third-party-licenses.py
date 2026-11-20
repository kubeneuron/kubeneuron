#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Dependency legal-file collector v1 (stdlib only, no unpinned tool downloads).

Collect the union of production dependencies for every supported OS/architecture.
Preserve upstream legal files verbatim, including nested vendored notices, and
record the SPDX identifiers each legal file's text matches. The licence gate
fails unless every identifier of every module is on ALLOWED (or the file is a
reviewed EXCEPTION): GPL/LGPL/AGPL/MPL/proprietary/UNKNOWN stop the build.
Detection is by the licence text itself, so a file that matches no known text
is UNKNOWN, never assumed permissive.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
COMMANDS = [f'./cmd/{name}' for name in (
    'kubeneuron-operator', 'kubeneuron-controller', 'kubeneuron-agent', 'kubeneuronctl')]
PLATFORMS = ('linux/amd64', 'linux/arm64', 'darwin/amd64', 'darwin/arm64')
# Licences a dependency may carry. The Go patent grant is allowed only beside
# BSD-3-Clause, the licence it accompanies in the Go repositories.
ALLOWED = {'Apache-2.0', 'BSD-2-Clause', 'BSD-3-Clause', 'MIT', 'ISC', '0BSD', 'Zlib', 'BSL-1.0'}
XZ_MODULE = 'github.com/xi2/xz@v0.0.0-20171230120015-48954b6210f8'
XZ_LICENSE_SHA256 = '5c640056d513f276d56635af7a0bed81cb7eac02a31b6e56d40c416557a8d8ce'
XZ_LICENSE_REF = 'LicenseRef-xi2-xz-public-domain'
PATENT_GRANT = 'Go-patent-grant'
# Reviewed exceptions: (module path, legal file) -> (sha256 of the reviewed
# content, why it is allowed). Bound to the content: if the file changes in a
# later version of the module, the gate fails again and asks for a review.
EXCEPTIONS = {
    ('modernc.org/memory', 'LICENSE-LOGO'): (
        '5ae5bee3072a841376451b48d8cfcec7188e10543926d5870828d36c8a750dc5',
        'attribution pointer for a logo image that is not compiled or shipped'),
}
# (SPDX id, predicate over whitespace-normalised text). A file may match more
# than one: dual-licensed files record every licence they grant.
SIGNATURES = [
    ('Apache-2.0', lambda t: 'Apache License' in t and 'Version 2.0' in t),
    ('MIT', lambda t: 'Permission is hereby granted, free of charge, to any person obtaining a copy' in t),
    ('BSD-3-Clause', lambda t: 'Redistribution and use in source and binary forms' in t and re.search(
        r'(Neither the name|names? of (its|the) contributors|name of the copyright holder).{0,200}(endorse|promote)', t, re.I)),
    ('BSD-2-Clause', lambda t: 'Redistribution and use in source and binary forms' in t and not re.search(
        r'(endorse|promote) products', t, re.I)),
    ('ISC', lambda t: re.search(r'Permission to use, copy, modify, and(/or)? distribute this software for any purpose with or without fee '
                                r'is hereby granted, provided that the above copyright notice and this permission notice appear in all copies', t)),
    ('0BSD', lambda t: re.search(r'Permission to use, copy, modify, and(/or)? distribute this software for any purpose with or without fee '
                                 r'is hereby granted\.', t)),
    ('Zlib', lambda t: "This software is provided 'as-is', without any express or implied warranty" in t
     and 'Permission is granted to anyone to use this software for any purpose' in t),
    ('BSL-1.0', lambda t: 'Boost Software License' in t),
    ('MPL-2.0', lambda t: 'Mozilla Public License' in t),
    ('GPL-family', lambda t: re.search(r'GNU (AFFERO |LESSER |LIBRARY )?GENERAL PUBLIC LICENSE', t, re.I)),
    ('CC', lambda t: re.search(r'Creative Commons', t, re.I)),
    (PATENT_GRANT, lambda t: 'Additional IP Rights Grant (Patents)' in t),
]
LEGAL = re.compile(r'^(licen[cs]e|copying|notice|copyright|patents)([-_][^.]+)?(\.(txt|md|rst|html|libyaml|bsd|mit|apache|gpl))?$', re.I)


def classify(text):
    """SPDX identifiers whose licence text appears in text; ['UNKNOWN'] if none."""
    normalised = ' '.join(text.split())
    found = sorted({spdx for spdx, matches in SIGNATURES if matches(normalised)})
    return found or ['UNKNOWN']


def problems(module, licenses, digests=None):
    """Why a module's licences fail the gate; empty when they pass. digests
    maps a legal file to the sha256 of its content, for the exceptions."""
    path = module.split('@')[0]
    found, issues = set(), []
    if path == 'github.com/xi2/xz':
        if module != XZ_MODULE or (digests or {}).get('LICENSE') != XZ_LICENSE_SHA256:
            issues.append('xi2/xz version or LICENSE changed since review')
    for name, ids in licenses.items():
        exception = EXCEPTIONS.get((path, name))
        if exception and (digests or {}).get(name) == exception[0]:
            continue
        if exception:
            issues.append(f'{name} changed since its licence exception was reviewed')
        found.update(ids)
    if not found:
        # Only NOTICE files, or only a reviewed exception: there is no licence
        # text at all, which is worse than an unknown one.
        issues.append('no licence text found')
    allowed = ALLOWED
    if module == XZ_MODULE and (digests or {}).get('LICENSE') == XZ_LICENSE_SHA256:
        allowed = ALLOWED | {XZ_LICENSE_REF}
    for spdx in sorted(found - allowed - {PATENT_GRANT}):
        issues.append(f'{spdx} is not an allowed licence')
    if PATENT_GRANT in found and 'BSD-3-Clause' not in found:
        issues.append('Go patent grant without the BSD-3-Clause licence it accompanies')
    return issues


def go(*args, env=None):
    return subprocess.check_output([os.environ.get('GO', 'go'), *args], cwd=ROOT,
                                   env=env, text=True)


def objects(raw):
    decoder = json.JSONDecoder()
    while raw.strip():
        raw = raw.lstrip()
        obj, end = decoder.raw_decode(raw)
        yield obj
        raw = raw[end:]


def generate():
    modules = {}
    for platform in PLATFORMS:
        goos, goarch = platform.split('/')
        env = dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED='0')
        for package in objects(go('list', '-mod=readonly', '-deps', '-json', *COMMANDS, env=env)):
            module = package.get('Module')
            if not module or module.get('Main'):
                continue
            actual = module.get('Replace', module)
            identity = module['Path'] + '@' + actual.get('Version', 'local')
            modules[identity] = Path(actual['Dir'])
    modules['Go standard library'] = Path(go('env', 'GOROOT').strip())
    chunks = ['Generated by hack/third-party-licenses.py v1; do not edit.\n'
              'Production dependency union: ' + ', '.join(PLATFORMS) + '\n']
    notices = ['KubeNeuron\nCopyright 2026 Andrii Petruk\n\n'
               'This product includes software developed by Andrii Petruk and the KubeNeuron contributors (https://github.com/kubeneuron/kubeneuron).\n\n'
               'Third-party notices (generated by hack/third-party-licenses.py v1).\n'
               'Full upstream license texts are in THIRD_PARTY_LICENSES.\n']
    inventory = []
    for identity, directory in sorted(modules.items()):
        # GOROOT contains test fixtures under src; its root license covers stdlib.
        paths = (directory.iterdir() if identity == 'Go standard library'
                 else directory.rglob('*'))
        files = sorted(p for p in paths if p.is_file() and LEGAL.match(p.name)
                       and not any(part == 'testdata' or part.startswith(('_', '.'))
                                   for part in p.relative_to(directory).parts))
        if not files:
            raise RuntimeError(f'No legal files for {identity}')
        licenses, digests = {}, {}
        for path in files:
            raw = path.read_bytes()
            data = raw.decode('utf-8')
            name = str(path.relative_to(directory))
            label = f'{identity}/{name}'
            chunks.append(f'\n===== {label} =====\n{data}\n')
            if path.name.lower().startswith('notice'):
                # Attribution, not a grant: kept verbatim, not classified.
                notices.append(f'\n===== {label} =====\n{data}\n')
            else:
                licenses[name] = classify(data)
                digests[name] = hashlib.sha256(raw).hexdigest()
                if identity == XZ_MODULE and name == 'LICENSE' and digests[name] == XZ_LICENSE_SHA256:
                    licenses[name] = [XZ_LICENSE_REF]
        spdx = sorted({i for ids in licenses.values() for i in ids})
        inventory.append({'module': identity, 'files': [str(p.relative_to(directory)) for p in files],
                          'spdx': spdx, 'licenses': licenses, 'review': problems(identity, licenses, digests)})
    return {'THIRD_PARTY_LICENSES': ''.join(chunks), 'NOTICE': ''.join(notices),
            'third-party-inventory.json': json.dumps(inventory, indent=2) + '\n'}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--check', action='store_true')
    args = parser.parse_args()
    stale = []
    outputs = generate()
    blocked = [f"{entry['module']}: {'; '.join(entry['review'])}"
               for entry in json.loads(outputs.get('third-party-inventory.json', '[]')) if entry['review']]
    if blocked:
        sys.exit('Licence gate: dependencies with licences outside the allowlist:\n  ' + '\n  '.join(blocked))
    for name, content in outputs.items():
        path = ROOT / name
        if args.check:
            if not path.exists() or path.read_text() != content:
                stale.append(name)
        else:
            path.write_text(content)
    if stale:
        sys.exit('Stale third-party inventory; run make licenses: ' + ', '.join(stale))


if __name__ == '__main__':
    main()
