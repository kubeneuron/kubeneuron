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


def embedded_script(text):
    # The CronJob's shell script: the block under "args: - |".
    m = re.search(r"^(\s+)args:\n\s+- \|\n((?:\1\s+.*\n|\n)+)", text, re.MULTILINE)
    if not m:
        raise ValueError("the CronJob script (args: - |) not found")
    lines = m.group(2).splitlines()
    indent = min(len(l) - len(l.lstrip()) for l in lines if l.strip())
    return "\n".join(l[indent:] for l in lines) + "\n"


def check_cronjob_download(text):
    # The download must not write the dated copy itself: an interrupted
    # transfer would leave a truncated file that retention counts as a copy.
    errors = []
    try:
        script = embedded_script(text)
    except ValueError as e:
        return [str(e)]
    targets = re.findall(r'-o "\$(\w+)"', script)
    names = dict(re.findall(r'^(\w+)="([^"]*)"', script, re.MULTILINE))
    if len(targets) != 1:
        return [f"want one curl -o target in the script, found {len(targets)}"]
    target = targets[0]
    final = names.get("out", "")
    if target == "out" or names.get(target, "").startswith("/backups/kubeneuron-"):
        errors.append(f"curl writes ${target}, a name the retention counts; download to a temporary name")
    if not names.get(target, "").startswith("/backups/."):
        errors.append(f"the download target ${target} is not a hidden name in /backups")
    move = script.find(f'mv "${target}" "$out"')
    verify = script.find(f'whole_db "${target}"')
    if move < 0:
        errors.append(f"the verified download is not renamed with mv \"${target}\" \"$out\"")
    elif verify < 0 or verify > move:
        errors.append(f"${target} is not checked with whole_db before the rename")
    if not final.startswith("/backups/kubeneuron-"):
        errors.append("the dated copy name no longer matches the retention pattern")
    result = subprocess.run(["sh", "-n"], input=script, capture_output=True, text=True)
    if result.returncode != 0:
        errors.append(f"sh -n: {result.stderr.strip()}")
    return errors


def marked(text, marker):
    # Lines of YAML (not comments) that carry the marker.
    return sum(1 for line in text.splitlines() if marker in line and not line.lstrip().startswith("#"))


# Names in the templates that do not depend on the installation.
FIXED_NAMES = ("kubeneuron-backups", "kubeneuron-backup", "kubeneuron-restore-helper",
               "kubeneuron-public-ca", "/backups/kubeneuron-", "/backups/.kubeneuron-", "app.kubernetes.io/name: kube-neuron")


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
    errors += [f"{CRONJOB.name}: {e}" for e in check_cronjob_volume(cronjob) + check_cronjob_download(cronjob)]
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
