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
            license_text = 'Apache License\nVersion 2.0, January 2004\nCopyright example\n'
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
            # The fixture's nested files are not real licence texts: UNKNOWN,
            # and the gate says so.
            self.assertEqual(inventory[1]['spdx'], ['Apache-2.0', 'UNKNOWN'])
            self.assertEqual(inventory[1]['licenses']['LICENSE'], ['Apache-2.0'])
            self.assertNotIn('NOTICE', inventory[1]['licenses'])
            self.assertEqual(inventory[1]['review'], ['UNKNOWN is not an allowed licence'])

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


APACHE = 'Apache License\nVersion 2.0, January 2004\n'
MIT = 'Permission is hereby granted, free of charge, to any person obtaining a copy\nof this software'
BSD3 = ('Redistribution and use in source and binary forms, with or without modification, are permitted. '
        'Neither the name of Google Inc. nor the names of its contributors may be used to endorse or promote products')
GPL = 'GNU GENERAL PUBLIC LICENSE\nVersion 3, 29 June 2007\n'
PATENTS = 'Additional IP Rights Grant (Patents)\n'


class GateTest(unittest.TestCase):
    def test_classifies_known_texts(self):
        self.assertEqual(collector.classify(APACHE), ['Apache-2.0'])
        self.assertEqual(collector.classify(MIT), ['MIT'])
        self.assertEqual(collector.classify(BSD3), ['BSD-3-Clause'])
        self.assertEqual(collector.classify(APACHE + MIT), ['Apache-2.0', 'MIT'])
        self.assertEqual(collector.classify('some bespoke terms'), ['UNKNOWN'])

    def test_allowed_set_passes(self):
        for ids in (['Apache-2.0'], ['MIT'], ['BSD-3-Clause'], ['BSD-2-Clause'], ['ISC'], ['0BSD'],
                    ['Zlib'], ['BSL-1.0'], ['Apache-2.0', 'MIT']):
            with self.subTest(ids=ids):
                self.assertEqual(collector.problems('example.org/dep@v1', {'LICENSE': ids}), [])
        self.assertEqual(collector.problems('golang.org/x/net@v1',
                                            {'LICENSE': ['BSD-3-Clause'], 'PATENTS': ['Go-patent-grant']}), [])

    def test_disallowed_and_unknown_fail(self):
        for text, want in ((GPL, 'GPL-family'), ('Mozilla Public License Version 2.0', 'MPL-2.0'),
                           ('All rights reserved. Proprietary.', 'UNKNOWN')):
            with self.subTest(want=want):
                issues = collector.problems('example.org/dep@v1', {'LICENSE': collector.classify(text)})
                self.assertTrue(issues and want in issues[0], issues)
        # Every licence of a dual-licensed module must be allowed.
        self.assertTrue(collector.problems('example.org/dep@v1', {'LICENSE': ['Apache-2.0'], 'COPYING': ['GPL-family']}))

    def test_no_licence_text_fails(self):
        reviewed = collector.EXCEPTIONS[('modernc.org/memory', 'LICENSE-LOGO')][0]
        self.assertEqual(collector.problems('example.org/dep@v1', {}), ['no licence text found'])
        self.assertEqual(collector.problems('modernc.org/memory@v1.11.0', {'LICENSE-LOGO': ['UNKNOWN']},
                                            {'LICENSE-LOGO': reviewed}), ['no licence text found'])

    def test_patent_grant_needs_bsd3(self):
        self.assertTrue(collector.problems('example.org/dep@v1', {'LICENSE': ['MIT'], 'PATENTS': [collector.PATENT_GRANT]}))

    def test_reviewed_exception_is_skipped_only_for_its_file_and_content(self):
        reviewed = collector.EXCEPTIONS[('modernc.org/memory', 'LICENSE-LOGO')][0]
        licences = {'LICENSE': ['BSD-3-Clause'], 'LICENSE-LOGO': ['UNKNOWN']}
        self.assertEqual(collector.problems('modernc.org/memory@v1.11.0', licences, {'LICENSE-LOGO': reviewed}), [])
        changed = collector.problems('modernc.org/memory@v1.11.0', licences, {'LICENSE-LOGO': 'f' * 64})
        self.assertTrue(any('changed since its licence exception was reviewed' in i for i in changed), changed)
        self.assertTrue(collector.problems('modernc.org/memory@v1.11.0',
                                           {'LICENSE': ['BSD-3-Clause'], 'LICENSE-OTHER': ['UNKNOWN']}))

    def test_gate_stops_the_build_with_the_module_name(self):
        inventory = json.dumps([{'module': 'example.org/gpl@v1', 'review': ['GPL-family is not an allowed licence']}])
        with patch.object(collector, 'generate', return_value={'third-party-inventory.json': inventory}), \
                patch('sys.argv', ['collector', '--check']):
            with self.assertRaises(SystemExit) as stop:
                collector.main()
        self.assertIn('example.org/gpl@v1', str(stop.exception.code))


if __name__ == '__main__':
    unittest.main()
