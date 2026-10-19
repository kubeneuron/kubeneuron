#!/usr/bin/env python3
"""Render the shipped Alertmanager configuration for the kind fixture.

Reads spec.configRawYaml from the checked-in VMAlertmanager resource
(deploy/kubernetes/dependencies/observability/vmalertmanager.yaml) and writes
an Alertmanager config file whose `route`, `inhibit_rules` and `kubeneuron`
receiver are the shipped objects, untouched. The one deliberate difference is
test-only transport: the shipped `human-default` receiver has no delivery
integration (it is the placeholder operators replace with their pager), so
nothing could observe what reaches it. The fixture gives it exactly one
webhook pointing at a local recorder sink, and nothing else. If the shipped
`human-default` ever gains an integration, this script refuses rather than
silently layering a second one on top; update the fixture then.

Usage:
  extract-alertmanager-config.py <vmalertmanager.yaml> <human-webhook-url> \
      [--contract <contract.json>]

The optional contract file receives the facts the harness has to satisfy for
the untouched `kubeneuron` receiver to deliver: the controller webhook URL
(the harness fronts it with a recorder sink of the same Service name,
namespace and port) and the bearer-token file path (the harness mounts a
fixture token there).
"""
import json
import sys
from urllib.parse import urlsplit

import yaml

HUMAN = "human-default"
CONTROLLER = "kubeneuron"


def die(msg: str) -> "NoReturn":  # noqa: F821 - typing not imported on purpose
    sys.exit(f"extract-alertmanager-config: {msg}")


def main(argv: list[str]) -> None:
    contract_path = None
    args = list(argv[1:])
    if "--contract" in args:
        i = args.index("--contract")
        try:
            contract_path = args[i + 1]
        except IndexError:
            die("--contract needs a path")
        del args[i : i + 2]
    if len(args) != 2:
        die(__doc__)
    cr_path, human_url = args

    docs = [d for d in yaml.safe_load_all(open(cr_path, encoding="utf-8")) if d]
    cr = [d for d in docs if d.get("kind") == "VMAlertmanager"]
    if len(cr) != 1:
        die(f"{cr_path}: want exactly one VMAlertmanager, found {len(cr)}")
    raw = cr[0].get("spec", {}).get("configRawYaml")
    if not isinstance(raw, str) or not raw.strip():
        die(f"{cr_path}: spec.configRawYaml missing")
    cfg = yaml.safe_load(raw)
    for key in ("route", "receivers"):
        if key not in cfg:
            die(f"configRawYaml has no {key}")

    receivers = cfg["receivers"]
    names = [r.get("name") for r in receivers]
    if names.count(HUMAN) != 1 or names.count(CONTROLLER) != 1:
        die(f"receivers must name {HUMAN} and {CONTROLLER} exactly once, got {names}")
    human = next(r for r in receivers if r["name"] == HUMAN)
    if set(human) != {"name"}:
        die(
            f"shipped {HUMAN} receiver now carries {sorted(set(human) - {'name'})}: "
            "the fixture's test-only webhook substitution assumes it has no delivery "
            "integration; update the fixture before changing this"
        )
    controller = next(r for r in receivers if r["name"] == CONTROLLER)
    hooks = controller.get("webhook_configs")
    if not isinstance(hooks, list) or len(hooks) != 1:
        die(f"shipped {CONTROLLER} receiver must have exactly one webhook_configs entry")
    hook = hooks[0]
    url = hook.get("url")
    cred = hook.get("http_config", {}).get("authorization", {}).get("credentials_file")
    if not url or not cred:
        die(f"shipped {CONTROLLER} webhook needs url and http_config.authorization.credentials_file")
    parts = urlsplit(url)
    if parts.scheme != "http" or not parts.hostname or not parts.port or not parts.path:
        die(f"controller webhook url {url!r}: want http://<service>.<namespace>.svc:<port>/<path>")
    host_labels = parts.hostname.split(".")
    if len(host_labels) != 3 or host_labels[2] != "svc":
        die(f"controller webhook host {parts.hostname!r}: want <service>.<namespace>.svc")
    if not cred.startswith("/") or cred.count("/") < 2:
        die(f"credentials_file {cred!r}: want an absolute path inside a mount directory")

    # The substitution: one webhook on human-default, nothing else changes.
    human["webhook_configs"] = [{"url": human_url, "send_resolved": True}]

    yaml.safe_dump(cfg, sys.stdout, sort_keys=False, default_flow_style=False)

    if contract_path:
        mount_dir, _, key = cred.rpartition("/")
        contract = {
            "controller_url": url,
            "controller_service": host_labels[0],
            "controller_namespace": host_labels[1],
            "controller_port": parts.port,
            "controller_path": parts.path,
            "credentials_file": cred,
            "credentials_mount_dir": mount_dir,
            "credentials_secret": mount_dir.rsplit("/", 1)[-1],
            "credentials_key": key,
            "human_webhook_url": human_url,
        }
        with open(contract_path, "w", encoding="utf-8") as f:
            json.dump(contract, f, indent=2, sort_keys=True)
            f.write("\n")


if __name__ == "__main__":
    main(sys.argv)
