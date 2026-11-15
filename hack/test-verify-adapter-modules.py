#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Regression checks for the contract dependency guard, using local modules."""

from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


class DependencyGuardTest(unittest.TestCase):
    def test_dependency_graphs(self):
        prefix = "github.com/kubeneuron/kubeneuron"
        for dependency in (None, prefix, prefix + "/pkg/cloud", prefix + "/pkg/accelerator-extra"):
            with self.subTest(dependency=dependency), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                (root / "hack").mkdir()
                license_text = Path(__file__).resolve().parent.parent / "LICENSE"
                shutil.copyfile(license_text, root / "LICENSE")
                script = root / "hack/verify-adapter-modules.py"
                shutil.copyfile(Path(__file__).with_name(script.name), script)
                for name in ("accelerator", "cloud"):
                    module = root / "pkg" / name
                    module.mkdir(parents=True)
                    shutil.copyfile(license_text, module / "LICENSE")
                    (module / "go.mod").write_text(f"module {prefix}/pkg/{name}\n\ngo 1.22\n")
                    (module / "contract.go").write_text(f'package {name}\nimport _ "context"\n')
                if dependency:
                    external = root / "external"
                    external.mkdir()
                    (external / "go.mod").write_text(f"module {dependency}\n\ngo 1.22\n")
                    (external / "external.go").write_text("package external\n")
                    module = root / "pkg/accelerator"
                    with (module / "go.mod").open("a") as file:
                        file.write(f"\nrequire {dependency} v0.0.0\nreplace {dependency} => ../../external\n")
                    # Only tests import it: production-only graph checks miss it.
                    (module / "contract_test.go").write_text(f'package accelerator\nimport _ "{dependency}"\n')
                result = subprocess.run(
                    ["python3", "-I", str(script)], capture_output=True, text=True,
                )
                if dependency:
                    self.assertNotEqual(result.returncode, 0, result.stdout)
                    self.assertIn("non-stdlib dependency", result.stderr)
                    self.assertIn(dependency, result.stderr)
                else:
                    self.assertEqual(result.returncode, 0, result.stderr)

    def test_missing_or_changed_license(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "hack").mkdir()
            module = root / "pkg/accelerator"
            module.mkdir(parents=True)
            script = root / "hack/verify-adapter-modules.py"
            shutil.copyfile(Path(__file__).with_name(script.name), script)
            shutil.copyfile(Path(__file__).resolve().parent.parent / "LICENSE", root / "LICENSE")
            for content in (None, "See the root license"):
                with self.subTest(content=content):
                    if content is not None:
                        (module / "LICENSE").write_text(content)
                    result = subprocess.run(["python3", "-I", str(script)], capture_output=True, text=True)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("LICENSE must match", result.stderr)


if __name__ == "__main__":
    unittest.main()
