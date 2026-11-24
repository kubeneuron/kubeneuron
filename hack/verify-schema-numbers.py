#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Compare migration heads with the largest schema numbers in current release docs."""

import re
import sys
from pathlib import Path


def check_section(path, heading, heads):
    text = path.read_text(encoding="utf-8")
    sections = re.finditer(heading, text, re.MULTILINE)
    start = next(sections, None)
    if not start:
        print(f"{path}: current release section not found", file=sys.stderr)
        return False
    while True:
        body = text[start.end():]
        section = re.split(r"^##[ \t]+", body, maxsplit=1, flags=re.MULTILINE)[0]
        if section.strip() or start[0].strip() != "## [Unreleased]":
            break
        start = next(sections, None)
        if start is None:
            return True
    valid = True
    for store, head in heads.items():
        mentions = list(re.finditer(rf"\b{store}\s+(\d{{4}})\b", section))
        if not mentions:
            continue
        largest = max(int(m[1]) for m in mentions)
        if largest == head:
            continue
        # Point to a future number itself, or the last mention in stale docs.
        last = mentions[-1] if largest < head else next(
            m for m in reversed(mentions) if int(m[1]) == largest
        )
        line = text.count("\n", 0, start.end() + last.start(1)) + 1
        print(f"{path}:{line}: highest {store} schema mentioned is {largest:04d}; "
              f"expected migration head {head:04d}", file=sys.stderr)
        valid = False
    return valid


def main():
    sqlite, postgres = sys.argv[1:]
    heads = {"SQLite": int(sqlite), "PostgreSQL": int(postgres)}
    upgrade = check_section(Path("docs/upgrade.md"), r"^## Upgrading from [^\n]+", heads)
    changelog = check_section(Path("CHANGELOG.md"), r"^## \[[^\]\n]+\][^\n]*", heads)
    return upgrade and changelog


if __name__ == "__main__":
    sys.exit(0 if main() else 1)
