#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Offline regression tests for the dependency legal-file collector."""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('collector', Path(__file__).with_name('third-party-licenses.py'))
collector = importlib.util.module_from_spec(spec)
spec.loader.exec_module(collector)


class CollectorTest(unittest.TestCase):
    def test_preserves_notices_and_platform_union(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            stdlib = root / 'go'
            stdlib.mkdir()
            (stdlib / 'LICENSE').write_text('Go license\n')
            dep = root / 'dep'
            dep.mkdir()
            license_text = 'Apache License\nCopyright example\n'
            (dep / 'LICENSE').write_text(license_text)
            (dep / 'NOTICE').write_text('upstream attribution\n')
            (dep / 'notice_response.go').write_text('not a legal file')
            (dep / 'nested').mkdir()
            (dep / 'nested' / 'COPYING').write_text('nested license\n')
            (dep / 'LICENSE.libyaml').write_text('libyaml license\n')
            for name in ('testdata', '_fixture', '.fixture', 'vendor', 'third_party'):
                (dep / name).mkdir()
                (dep / name / 'LICENSE').write_text(f'{name} legal text\n')
            seen = []

            def fake_go(*args, env=None):
                if args == ('env', 'GOROOT'):
                    return str(stdlib)
                seen.append((env['GOOS'], env['GOARCH']))
                return json.dumps({'Module': {'Path': 'example.org/dep', 'Version': 'v1.0.0', 'Dir': str(dep)}})

            with patch.object(collector, 'go', side_effect=fake_go):
                result = collector.generate()
            self.assertEqual(len(seen), 4)
            self.assertIn(license_text, result['THIRD_PARTY_LICENSES'])
            self.assertIn('nested license\n', result['THIRD_PARTY_LICENSES'])
            self.assertIn('libyaml license\n', result['THIRD_PARTY_LICENSES'])
            for name in ('testdata', '_fixture', '.fixture'):
                self.assertNotIn(f'{name} legal text', result['THIRD_PARTY_LICENSES'])
            for name in ('vendor', 'third_party'):
                self.assertIn(f'{name} legal text', result['THIRD_PARTY_LICENSES'])
            self.assertIn('upstream attribution\n', result['NOTICE'])
            self.assertNotIn(str(root), result['THIRD_PARTY_LICENSES'])
            self.assertNotIn('not a legal file', result['THIRD_PARTY_LICENSES'])
            inventory = json.loads(result['third-party-inventory.json'])
            self.assertEqual(len(inventory), 2)
            self.assertEqual(inventory[1]['review'], [])

    def test_check_rejects_missing_or_changed_output(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with patch.object(collector, 'ROOT', root), patch.object(collector, 'generate', return_value={'NOTICE': 'expected'}), patch('sys.argv', ['collector', '--check']):
                with self.assertRaises(SystemExit):
                    collector.main()
                (root / 'NOTICE').write_text('stale')
                with self.assertRaises(SystemExit):
                    collector.main()
                (root / 'NOTICE').write_text('expected')
                collector.main()


if __name__ == '__main__':
    unittest.main()
