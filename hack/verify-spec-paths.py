#!/usr/bin/env python3
"""Every `spec.a.b.c` path a published doc names must exist in a generated CRD.

An invented field is the most expensive kind of documentation defect: it reads
as authoritative, an operator acts on it, and the action silently does nothing.
docs/upgrade.md named `spec.execution.confinement` twice — in the section
written specifically to make a blast-radius change safe to upgrade through —
and every other check in this repository passed.
"""
import glob, json, os, re, sys, yaml

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
schemas = []
for f in glob.glob(os.path.join(root, "config/crd/bases/*.yaml")):
    crd = yaml.safe_load(open(f))
    for ver in crd["spec"]["versions"]:
        schema_paths(ver["schema"]["openAPIV3Schema"], "", known)
        schemas.append(ver["schema"]["openAPIV3Schema"])

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

# --- the same question, asked of the JSON an operator actually copies --------
#
# The dotted-prose check above cannot see a patch body, and a patch body is the
# form readers paste into a terminal. docs/pilot-checklist.md carried
#
#     -p '{"spec": {"agent": {"hostTooling": {"enabled": true}}}}'
#
# for as long as that section existed. HostToolingSpec has no `enabled` field:
# the CRD's structural schema PRUNES it, so the patch applies, kubectl reports
# success, and the reader is left believing they set a switch that was silently
# discarded. Worse than a no-op — a reader who writes `"enabled": false` to turn
# host tooling OFF gets `hostTooling: {}`, which turns it ON.
#
# Walking the schema rather than a path set is what makes this precise: a map
# field (nodeSelector, labels, tolerations' arbitrary keys) legitimately holds
# names no schema knows, so descending stops wherever the schema stops naming
# things.
def walk_body(node, obj, prefix, where, out):
    if not isinstance(obj, dict) or not isinstance(node, dict):
        return
    props = node.get("properties") or {}
    # A free-form map (additionalProperties) or a preserve-unknown island holds
    # user-chosen keys; below here the API has no opinion and neither have we.
    if not props:
        return
    for key, child in obj.items():
        path = f"{prefix}.{key}" if prefix else key
        if key not in props:
            if any(path == e or path.startswith(e + ".") for e in exceptions):
                continue
            out.append(f"{where}: {path} is not a field of this CRD; a structural schema PRUNES it silently")
            continue
        sub = props[key]
        if sub.get("type") == "array":
            items = sub.get("items") or {}
            for elem in obj[key] if isinstance(obj[key], list) else []:
                walk_body(items, elem, path, where, out)
            continue
        walk_body(sub, child, path, where, out)

# Balanced-brace scan: every {...} in the doc that mentions "spec" and parses
# as JSON. Shell quoting, indentation and line breaks all vary; brace balance
# does not.
def json_objects(text):
    depth = start = 0
    for i, ch in enumerate(text):
        if ch == "{":
            if depth == 0:
                start = i
            depth += 1
        elif ch == "}" and depth:
            depth -= 1
            if depth == 0:
                yield start, text[start:i + 1]

for doc in sorted(set(docs)):
    if os.path.basename(doc) in skip:
        continue
    text = open(doc).read()
    for offset, blob in json_objects(text):
        if '"spec"' not in blob:
            continue
        try:
            obj = json.loads(blob)
        except ValueError:
            continue  # a template with placeholders, not a literal patch
        if not isinstance(obj, dict) or "spec" not in obj:
            continue
        line = text.count("\n", 0, offset) + 1
        rel = os.path.relpath(doc, root)
        for schema in schemas:
            spec = (schema.get("properties") or {}).get("spec")
            if spec and all(k in (spec.get("properties") or {}) for k in obj["spec"]):
                walk_body(spec, obj["spec"], "spec", f"{rel}:{line}", bad)
                break

if bad:
    print("INVENTED SPEC PATHS (a doc names a field the API does not have):", file=sys.stderr)
    for b in sorted(set(bad)):
        print("  " + b, file=sys.stderr)
    sys.exit(1)
print(f"spec-paths: OK ({len(known)} CRD paths known; patch bodies walked against the schema)")
