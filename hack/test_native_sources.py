#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
import hashlib
import importlib.util
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('bundle', Path(__file__).with_name('bundle-native-sources.py'))
bundle = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bundle)


class SourceChecks(unittest.TestCase):
    def test_cached_source_requires_matching_hash(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'source.tar').write_bytes(b'correct')
            resource = dict(name='source.tar', url='https://example.invalid/source',
                            sha256=hashlib.sha256(b'correct').hexdigest())
            self.assertEqual(bundle.checked_bytes(resource, root), b'correct')
            (root / 'source.tar').write_bytes(b'tampered')
            with self.assertRaisesRegex(ValueError, 'checksum mismatch'):
                bundle.checked_bytes(resource, root)

    def test_path_escape_is_rejected(self):
        for name in ('../source.tar', '/source.tar'):
            with self.assertRaisesRegex(ValueError, 'Unsafe source path'):
                bundle.checked_bytes(dict(name=name))


if __name__ == '__main__':
    unittest.main()
