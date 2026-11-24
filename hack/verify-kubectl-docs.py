# SPDX-License-Identifier: Apache-2.0
"""Check literal kubectl examples against the default installation."""
import json
import re
import sys
from pathlib import Path

import yaml


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
        for i, (word, line) in enumerate(tokens):
            following = words[i + 1] if i + 1 < len(words) else ""
            if word in ("-n", "--namespace") or word.startswith("--namespace="):
                value = following if "=" not in word else word.split("=", 1)[1].strip("'\"")
                # Runtime namespaces may be supplied by the reader.
                if value in ("<ns>", "<namespace>"):
                    continue
                # External examples: NVIDIA exporter, VictoriaMetrics, SRE token.
                if ((value == "gpu-operator" and "ds/nvidia-dcgm-exporter" in words)
                    or (value == "gpu-operator" and "daemonset/nvidia-dcgm-exporter" in words)
                    or (value == "kubeneuron-monitoring" and "vmagent" in words)
                    or (value == "ops)" and "sre-bot" in words)):
                    continue
                if value != namespace:
                    errors.append((line, f"namespace {value!r}, expected {namespace!r}"))
            match = re.fullmatch(r"(?:deploy|deployment)/([^/]+)", word)
            name = match.group(1) if match else None
            if word in ("deploy", "deployment") and following and not following.startswith("-"):
                name = following
            # The operator generates controller and Grafana Deployments per installation.
            if name and name not in deployments:
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
    manifest = yaml.safe_load(Path("config/default/operator_deployment.yaml").read_text())
    meta = manifest["metadata"]
    operator = manifest["spec"]["template"]["metadata"]["labels"]
    rendered = json.loads(Path("internal/operator/testdata/kubectl-docs.json").read_text())
    labels = [operator, *rendered["pod_labels"]]
    deployments = {meta["name"], *rendered["deployments"]}
    for name in rendered["deployments"]:
        deployments.add(name.replace(rendered["installation"], "<name>", 1))
    paths = [Path(arg) for arg in sys.argv[1:]] or [Path("README.md"), *sorted(Path("docs").glob("*.md"))]
    errors = [error for path in paths for error in check(path, meta["namespace"], deployments, labels)]
    for error in errors:
        print(error, file=sys.stderr)
    return bool(errors)


if __name__ == "__main__":
    sys.exit(main())
