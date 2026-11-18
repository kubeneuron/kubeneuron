#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Print the metric names the checked-in dashboards query, one per line.

Reads every panel expression and query variable in deploy/grafana/*.json.
A metric name here is a kubeneuron_*, DCGM_FI_* or `up` token outside
label matchers and quoted strings: every series the boards read is named
that way, and label names (by (class, le), "service.name") never are.
hack/test-managed-observability.sh checks each one against the Managed
store.
"""

import glob
import json
import os
import re
import sys

NAME = re.compile(r"(?<![A-Za-z0-9_:.])(kubeneuron_[A-Za-z0-9_:]+|DCGM_FI_[A-Za-z0-9_]+|up)(?![A-Za-z0-9_:(])")
MATCHERS = re.compile(r"\{[^{}]*\}")
QUOTED = re.compile(r'"(?:[^"\\]|\\.)*"')


def expressions(node):
    if isinstance(node, dict):
        for key, value in node.items():
            if key == "expr" and isinstance(value, str):
                yield value
            elif key == "query" and isinstance(value, str):
                yield value
            elif key == "query" and isinstance(value, dict) and isinstance(value.get("query"), str):
                yield value["query"]
            else:
                yield from expressions(value)
    elif isinstance(node, list):
        for item in node:
            yield from expressions(item)


def names(expr):
    # Grafana variables ($node, ${DS_PROMETHEUS}) sit inside matchers or
    # strings; drop both, then read what is left.
    stripped = MATCHERS.sub(" ", QUOTED.sub(" ", expr))
    return set(NAME.findall(stripped))


def main():
    root = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "deploy", "grafana")
    boards = sorted(glob.glob(os.path.join(root, "*.json")))
    if len(boards) != 6:
        sys.exit(f"dashboard-series: want the six boards in deploy/grafana, found {len(boards)}")
    found = set()
    for board in boards:
        with open(board, encoding="utf-8") as f:
            for expr in expressions(json.load(f)):
                found |= names(expr)
    if not found:
        sys.exit("dashboard-series: no metric names found; the extractor is broken")
    print("\n".join(sorted(found)))


if __name__ == "__main__":
    main()
