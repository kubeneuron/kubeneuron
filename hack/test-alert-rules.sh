#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Unit tests of the Managed store's alert rules
# (configs/vmalert/gpu-alert-rules.test.yaml) with vmalert-tool, the
# VictoriaMetrics rule tester of the same release as the managed vmalert.
# Needs docker; no network once the image is present.
set -euo pipefail
REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
# Apache-2.0, referenced by digest, used only by this test; tag and digest in
# deploy/kubernetes/dependencies/versions.lock.yaml (victoriaMetricsAlertTool).
IMAGE=${VMALERT_TOOL_IMAGE:-victoriametrics/vmalert-tool:v1.147.0@sha256:b89411be0487fbf0d966047dd76657c18f23522c4e547ae1ac757d644c52335a}
docker run --rm --network none -v "$REPO_ROOT/configs/vmalert:/rules:ro" -w /rules "$IMAGE" \
	unittest --files=/rules/gpu-alert-rules.test.yaml
