# SPDX-License-Identifier: Apache-2.0
"""Check literal kubectl examples against the default installation."""
import json
import re
import sys
from pathlib import Path

# External components used in examples, scoped to their namespace and resource.
EXTERNAL_OBJECTS = {
    "gpu-operator": (("ds/nvidia-dcgm-exporter",), ("daemonset/nvidia-dcgm-exporter",)),
    "kubeneuron-monitoring": (("vmagent", "kubeneuron"),),
    "ops": (("create", "token", "sre-bot"),),
    "kube-system": (("deployment/coredns",),),
}


def commands(text):
    # Keep the original line of each argument, including in continued commands.
    lines = text.splitlines()
    for index, line in enumerate(lines):
        for start in re.finditer(r"\bkubectl\s+", line):
            fragment = line[start.end():]
            inline = line[:start.start()].count("`") % 2
            current = index
            tokens = []
            while True:
                if line[:start.start()].endswith("$("):
                    fragment = fragment.split(")", 1)[0]
                part = re.split(r"[`|;]|\s+#", fragment, maxsplit=1)[0]
                tokens.extend((m.group().strip("'\x22"), current + 1)
                              for m in re.finditer(r"[^\s\\]+", part))
                if (current + 1 >= len(lines) or part != fragment or
                        not (part.rstrip().endswith("\\") or inline)):
                    break
                current += 1
                fragment = lines[current]
            yield tokens


def check(path, namespace, deployments, labels):
    errors = []
    for tokens in commands(path.read_text()):
        words = [word for word, _ in tokens]
        command_namespace = ""
        for index, word in enumerate(words):
            if word in ("--namespace", "-n") and index + 1 < len(words):
                command_namespace = words[index + 1]
            elif word.startswith("--namespace="):
                command_namespace = word.split("=", 1)[1].strip("'\"")
        for i, (word, line) in enumerate(tokens):
            following = words[i + 1] if i + 1 < len(words) else ""
            if word in ("-n", "--namespace") or word.startswith("--namespace="):
                value = following if "=" not in word else word.split("=", 1)[1].strip("'\"")
                # Runtime namespaces may be supplied by the reader.
                if value in ("<ns>", "<namespace>"):
                    continue
                external = EXTERNAL_OBJECTS.get(value, ())
                if any(list(resource) == words[pos:pos + len(resource)]
                       for resource in external for pos in range(len(words))):
                    continue
                if value != namespace:
                    errors.append((line, f"namespace {value!r}, expected {namespace!r}"))
            match = re.fullmatch(r"(?:deploy|deployment)/([^/]+)", word)
            name = match.group(1) if match else None
            if word in ("deploy", "deployment") and following and not following.startswith("-"):
                name = following
            # The operator generates controller and Grafana Deployments per installation.
            external_names = EXTERNAL_OBJECTS.get(command_namespace, ())
            if name and name not in deployments and (word,) not in external_names:
                errors.append((line, f"unknown Deployment {name!r}"))
            if word in ("-l", "--selector") or word.startswith(("--selector=", "-l=")):
                inline = "=" in word
                selector = word.split("=", 1)[1].strip("'\"") if inline else following
                if selector.startswith("app.kubernetes.io/"):
                    pairs = [pair.partition("=") for pair in selector.split(",")]
                    if not any(all(equal and candidate.get(key) == value
                                   for key, equal, value in pairs) for candidate in labels):
                        location = line if inline else tokens[i + 1][1]
                        errors.append((location, f"unknown pod selector {selector!r}"))
    return [f"{path}:{line}: {message}" for line, message in errors]


def main():
    rendered = json.loads(Path("internal/operator/testdata/kubectl-docs.json").read_text())
    operator = rendered["operator"]
    labels = [operator["pod_labels"], *rendered["pod_labels"]]
    deployments = {operator["name"], *rendered["deployments"]}
    for name in rendered["deployments"]:
        deployments.add(name.replace(rendered["installation"], "<name>", 1))
    paths = [Path(arg) for arg in sys.argv[1:]] or [Path("README.md"), *sorted(Path("docs").glob("*.md"))]
    errors = [error for path in paths for error in check(path, operator["namespace"], deployments, labels)]
    for error in errors:
        print(error, file=sys.stderr)
    return bool(errors)


if __name__ == "__main__":
    sys.exit(main())
