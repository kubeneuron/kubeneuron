#!/usr/bin/env python3
"""Every `spec.a.b.c` path a published doc names must exist in a generated CRD.

An invented field is the most expensive kind of documentation defect: it reads
as authoritative, an operator acts on it, and the action silently does nothing.
docs/upgrade.md named `spec.execution.confinement` twice — in the section
written specifically to make a blast-radius change safe to upgrade through —
and every other check in this repository passed.
"""
import glob, os, re, sys, yaml

root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

def schema_paths(node, prefix, out):
    """Collect every dotted path a CRD's openAPIV3Schema can address."""
    if not isinstance(node, dict):
        return
    for name, child in (node.get("properties") or {}).items():
        path = f"{prefix}.{name}" if prefix else name
        out.add(path)
        schema_paths(child, path, out)
        # A list of objects is addressed by the same dotted prose.
        if isinstance(child, dict) and child.get("type") == "array":
            schema_paths(child.get("items") or {}, path, out)

known = set()
for f in glob.glob(os.path.join(root, "config/crd/bases/*.yaml")):
    crd = yaml.safe_load(open(f))
    for ver in crd["spec"]["versions"]:
        schema_paths(ver["schema"]["openAPIV3Schema"], "", known)

# `spec.foo.bar` in prose maps to the CRD path without its leading "spec.".
docs = []
for pattern in ("docs/**/*.md", "*.md"):
    docs += glob.glob(os.path.join(root, pattern), recursive=True)
skip = {"CHANGELOG.md", "AGENT_SESSION_STATE.md", "TRANSFER_HANDOFF.md", "blocker.md"}

# A trailing word that is prose, not a field ("spec.safety.executionMode Enabled").
pathRe = re.compile(r"\bspec\.([a-zA-Z][a-zA-Z0-9]*(?:\.[a-zA-Z][a-zA-Z0-9]*)*)")

# Justified exceptions, each with its reason recorded beside it.
exceptions = set()
exc_file = os.path.join(root, "hack/spec-path-exceptions.txt")
if os.path.exists(exc_file):
    for line in open(exc_file):
        line = line.split("#")[0].strip()
        if line:
            exceptions.add(line)

bad = []
for doc in sorted(set(docs)):
    if os.path.basename(doc) in skip:
        continue
    for n, line in enumerate(open(doc), 1):
        for m in pathRe.finditer(line):
            path = "spec." + m.group(1)
            if any(path == e or path.startswith(e + ".") for e in exceptions):
                continue
            # Walk back to the longest prefix that IS known. Prose often
            # continues past the field ("spec.safety.executionMode Enabled"),
            # so a known prefix is enough; only a path whose FIRST segment is
            # unknown was invented.
            parts = path.split(".")
            while len(parts) > 1 and ".".join(parts) not in known:
                parts.pop()
            if len(parts) <= 1:
                rel = os.path.relpath(doc, root)
                bad.append(f"{rel}:{n}: {path} names no field in any generated CRD")

if bad:
    print("INVENTED SPEC PATHS (a doc names a field the API does not have):", file=sys.stderr)
    for b in bad:
        print("  " + b, file=sys.stderr)
    sys.exit(1)
print(f"spec-paths: OK ({len(known)} CRD paths known)")
