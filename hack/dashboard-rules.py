#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Print every dashboard panel expression as a recording-rule file.

Reads each panel `expr` in deploy/grafana/*.json, replaces the Grafana
variables ($node, $__rate_interval, ...) with a value a rule may carry and
prints one rule group per board, as JSON (which a YAML reader accepts).
hack/check-rules.sh hands the file to promtool and vmalert so the boards
are parsed by both. The label_values() variable queries are Grafana's
own syntax and are not included.
"""

import glob
import json
import os
import re
import sys

# $__rate_interval, $__interval, $__range: a duration.
DURATION = re.compile(r"\$\{?__(?:rate_interval|interval|range)(?::[a-z]+)?\}?")
# Any other $var or ${var}: a label value inside a matcher.
VARIABLE = re.compile(r"\$\{?[A-Za-z_][A-Za-z0-9_]*(?::[a-z]+)?\}?")


def expressions(node):
    if isinstance(node, dict):
        for key, value in node.items():
            if key == "expr" and isinstance(value, str):
                yield value
            else:
                yield from expressions(value)
    elif isinstance(node, list):
        for item in node:
            yield from expressions(item)


def main():
    root = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "deploy", "grafana")
    boards = sorted(glob.glob(os.path.join(root, "*.json")))
    if len(boards) != 6:
        sys.exit(f"dashboard-rules: want the six boards in deploy/grafana, found {len(boards)}")
    groups = []
    for board in boards:
        with open(board, encoding="utf-8") as f:
            exprs = [e for e in expressions(json.load(f)) if e.strip()]
        name = os.path.splitext(os.path.basename(board))[0]
        rules = []
        for i, expr in enumerate(exprs):
            expr = VARIABLE.sub(".*", DURATION.sub("5m", expr))
            rules.append({"record": f"dashboard:{name.replace('-', '_')}:{i}", "expr": expr})
        if not rules:
            sys.exit(f"dashboard-rules: no panel expressions in {name}; the extractor is broken")
        groups.append({"name": f"dashboard-{name}", "rules": rules})
    json.dump({"groups": groups}, sys.stdout, indent=1)
    print()


if __name__ == "__main__":
    main()
