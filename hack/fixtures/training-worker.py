# SPDX-License-Identifier: Apache-2.0
"""Two CPU training invocations share only the checkpoint on disk."""
import http.client
import json
import os
from pathlib import Path
import ssl
import sys

phase, host = sys.argv[1:]
state = Path("/tmp/training-checkpoint.json")
volume = Path("/var/run/training-report")


def report(**fact):
    tls = ssl.create_default_context(cafile=str(volume / "ca.crt"))
    conn = http.client.HTTPSConnection(host, 8444, context=tls, timeout=10)
    try:
        token = (volume / "token").read_text().strip()
        conn.request("POST", "/v1/training/reports", json.dumps(fact),
                     {"Authorization": "Bearer " + token, "Content-Type": "application/json"})
        status = conn.getresponse().status
        if status != 204:
            raise RuntimeError(f"training report returned HTTP {status}")
    finally:
        conn.close()


def update(weight):
    # One gradient step fitting y=2x to the sample x=1.
    return weight - 0.25 * (2 * (weight - 2))


if phase == "checkpoint":
    tmp = state.with_suffix(".tmp")
    tmp.write_text(json.dumps({"step": 0, "weight": update(0)}))
    os.replace(tmp, state)
    report(kind="checkpoint", checkpoint_id="epoch-0", step=0)
elif phase == "resume":
    saved = json.loads(state.read_text())
    if saved != {"step": 0, "weight": 1}:
        raise RuntimeError("unexpected checkpoint contents")
    report(kind="resume", checkpoint_id="epoch-0")
    weight = update(saved["weight"])
    if (weight - 2) ** 2 >= (saved["weight"] - 2) ** 2:
        raise RuntimeError("restored training step failed to reduce loss")
    report(kind="progress", step=saved["step"] + 1)
else:
    raise ValueError(phase)
