#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Exercise the native-component exclusion gate with Docker export fixtures."""
import io
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent
SCRIPT = (ROOT / 'hack/verify-image.sh').read_text()
GATE = SCRIPT[SCRIPT.index('# Export failures must'):SCRIPT.index('# Legal files must')]


class NativeExclusionTests(unittest.TestCase):
    def run_gate(self, paths=(), export_fails=False, corrupt=False):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            archive = root / 'image.tar'
            with tarfile.open(archive, 'w') as output:
                for path in paths:
                    entry = tarfile.TarInfo(path)
                    entry.size = 1
                    output.addfile(entry, io.BytesIO(b'x'))
            if corrupt:
                archive.write_bytes(b'invalid tar')
            docker = root / 'docker'
            docker.write_text('''#!/bin/sh
case "$1" in
create) echo fixture ;;
export) if [ "$EXPORT_FAILS" = 1 ]; then exit 1; fi; cat "$ARCHIVE" ;;
rm) exit 0 ;;
*) exit 2 ;;
esac
''')
            docker.chmod(0o755)
            env = dict(os.environ, DOCKER_BIN=str(docker), ARCHIVE=str(archive),
                       EXPORT_FAILS=str(int(export_fails)))
            return subprocess.run(['bash', '-c', '''set -euo pipefail
fail=0
agent_image=fixture
bad() { echo "IMAGE CHECK FAILED: $*" >&2; fail=1; }
''' + GATE + '\nexit "$fail"'], env=env, capture_output=True, text=True)

    def test_clean_image_passes(self):
        self.assertEqual(self.run_gate(['bin/nsenter', 'licenses/native/busybox/LICENSE']).returncode, 0)

    def test_forbidden_components_fail_in_any_directory(self):
        for path in ['usr/bin/dcgmi', 'usr/lib/aarch64-linux-gnu/libdcgm.so.4',
                     'lib/x86_64-linux-gnu/libgcc_s.so.1',
                     'licenses/native/datacenter-gpu-manager-4/LICENSE',
                     'licenses/native/libgcc-s1/copyright',
                     'licenses/native/dcgm/LICENSE']:
            with self.subTest(path=path):
                result = self.run_gate([path])
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('contains excluded', result.stderr)

    def test_export_failure_is_not_absence(self):
        result = self.run_gate(export_fails=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('could not export', result.stderr)

    def test_corrupt_archive_is_not_absence(self):
        result = self.run_gate(corrupt=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('could not list', result.stderr)


if __name__ == '__main__':
    unittest.main()
