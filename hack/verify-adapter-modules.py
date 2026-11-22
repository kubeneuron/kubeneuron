#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Check the active build and test dependency graphs of the Apache contracts."""

import os
from pathlib import Path
import subprocess
import sys


def main():
    root = Path(__file__).resolve().parent.parent
    env = dict(os.environ, GOWORK="off")
    for directory in ("pkg/accelerator", "pkg/cloud"):
        license_path = root / directory / "LICENSE"
        if not license_path.is_file() or license_path.read_bytes() != (root / "LICENSE").read_bytes():
            sys.exit(f"{directory}: LICENSE must match the full root Apache-2.0 LICENSE")
        module = "github.com/kubeneuron/kubeneuron/" + directory
        # Standard is Go's classification, not a heuristic based on dots in
        # import paths. Module identity also rejects sibling/root dependencies
        # and lookalike module prefixes. Include test-only dependencies.
        result = subprocess.run(
            [os.environ.get("GO", "go"), "list", "-mod=readonly", "-deps", "-test",
             "-f", "{{if not .Standard}}{{.ImportPath}}\t{{if .Module}}{{.Module.Path}}{{end}}{{end}}",
             "./..."],
            cwd=root / directory, env=env, check=True, capture_output=True, text=True,
        )
        for line in result.stdout.splitlines():
            if not line:
                continue
            package, _, owner = line.partition("\t")
            if owner != module:
                sys.exit(f"{directory}: non-stdlib dependency: {package} (module {owner})")
        print(f"{directory}: build and test dependencies are stdlib-only")


if __name__ == "__main__":
    try:
        main()
    except subprocess.CalledProcessError as error:
        sys.stderr.write(error.stderr)
        sys.exit(error.returncode)
