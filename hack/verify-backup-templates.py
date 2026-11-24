# SPDX-License-Identifier: Apache-2.0
"""Check that the backup templates follow the installation they are set for.

The CronJob and the restore helper name the installation and its namespace on
lines marked "# <name>" and "# <ns>". As shipped they must describe the
installer's default installation, and after the documented sed for another
installation every name derived from it must follow.
"""
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
CRONJOB = ROOT / "deploy/kubernetes/backup/backup-cronjob.yaml"
HELPER = ROOT / "deploy/kubernetes/backup/restore-helper.yaml"
DEFAULT_NAME, DEFAULT_NS = "kubeneuron", "kube-neuron"


def render(path, name, ns):
    # The command the template headers and docs/operations.md give.
    return subprocess.run(
        ["sed", "-e", f"/# <name>/s/kubeneuron/{name}/", "-e", f"/# <ns>/s/kube-neuron/{ns}/", str(path)],
        check=True, capture_output=True, text=True).stdout


def field(text, pattern, what):
    found = re.findall(pattern, text, re.MULTILINE)
    if not found:
        raise ValueError(f"{what} not found")
    return found


def check_cronjob(text, name, ns):
    errors = []
    installation = field(text, r"- name: INSTALLATION\n\s+value: (\S+)", "INSTALLATION")[0]
    secret = field(text, r"secretKeyRef:\n\s+name: (\S+)", "secretKeyRef name")[0]
    namespaces = set(field(text, r"^\s+namespace: (\S+)", "namespace"))
    if installation != name:
        errors.append(f"INSTALLATION is {installation}, want {name}")
    if secret != f"{name}-operator-api-token":
        errors.append(f"token Secret is {secret}, want {name}-operator-api-token")
    if namespaces != {ns}:
        errors.append(f"namespaces are {sorted(namespaces)}, want {ns}")
    if "$INSTALLATION-controller:" not in text:
        errors.append("the controller URL no longer derives from INSTALLATION")
    return errors


def check_helper(text, name):
    claim = field(text, r"claimName: (\S+)", "claimName")[0]
    if claim != f"{name}-controller-state":
        return [f"claimName is {claim}, want {name}-controller-state"]
    return []


def check_restricted(text):
    # The fields Pod Security "restricted" requires of these Pods.
    errors = []
    for want in ("runAsNonRoot: true", "type: RuntimeDefault", "allowPrivilegeEscalation: false", 'drop: ["ALL"]'):
        if want not in text:
            errors.append(f"missing {want!r} (Pod Security restricted)")
    return errors


def check_cronjob_volume(text):
    # A freshly formatted volume is owned by root; without fsGroup the
    # Job's non-root user cannot write the copy.
    errors = []
    for want in ("fsGroup: 101", "fsGroupChangePolicy: OnRootMismatch"):
        if not re.search(rf"^\s+{want}\s*$", text, re.MULTILINE):
            errors.append(f"missing {want!r} (the backup volume must be writable by uid 100)")
    return errors


def marked(text, marker):
    # Lines of YAML (not comments) that carry the marker.
    return sum(1 for line in text.splitlines() if marker in line and not line.lstrip().startswith("#"))


# Names in the templates that do not depend on the installation.
FIXED_NAMES = ("kubeneuron-backups", "kubeneuron-backup", "kubeneuron-restore-helper",
               "kubeneuron-public-ca", "/backups/kubeneuron-", "app.kubernetes.io/name: kube-neuron")


def unmarked(text):
    # A line that names the installation or its namespace without a marker
    # would not follow the documented sed.
    found = []
    for number, line in enumerate(text.splitlines(), 1):
        if line.lstrip().startswith("#") or "# <name>" in line or "# <ns>" in line:
            continue
        rest = line
        for fixed in FIXED_NAMES:
            rest = rest.replace(fixed, "")
        if "kubeneuron" in rest or "kube-neuron" in rest:
            found.append(f"line {number} names the installation without a marker: {line.strip()}")
    return found


def main():
    errors = []
    cronjob, helper = CRONJOB.read_text(), HELPER.read_text()
    for marker, count in (("# <name>", 2), ("# <ns>", 2)):
        if marked(cronjob, marker) != count:
            errors.append(f"{CRONJOB.name}: {marked(cronjob, marker)} lines marked {marker!r}, want {count}")
    if marked(helper, "# <name>") != 1:
        errors.append(f"{HELPER.name}: want one line marked '# <name>'")
    for path, text in ((CRONJOB, cronjob), (HELPER, helper)):
        errors += [f"{path.name}: {e}" for e in check_restricted(text) + unmarked(text)]
    errors += [f"{CRONJOB.name}: {e}" for e in check_cronjob_volume(cronjob)]
    for name, ns in ((DEFAULT_NAME, DEFAULT_NS), ("fleet", "ops")):
        try:
            errors += [f"{CRONJOB.name} for {name} in {ns}: {e}" for e in check_cronjob(render(CRONJOB, name, ns), name, ns)]
            errors += [f"{HELPER.name} for {name}: {e}" for e in check_helper(render(HELPER, name, ns), name)]
        except ValueError as e:
            errors.append(f"backup templates for {name}: {e}")
    for e in errors:
        print(f"verify-backup-templates: {e}", file=sys.stderr)
    if errors:
        return 1
    print("verify-backup-templates: OK (default installation and --name fleet --namespace ops)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
