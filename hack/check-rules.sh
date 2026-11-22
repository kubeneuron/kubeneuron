#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# The default rule pack and the dashboards are plain PromQL, checked on both
# backends: promtool parses every rule file and every dashboard panel
# expression, and so does vmalert -dryRun; the unit tests in
# configs/vmalert/gpu-promql-rules.test.yaml run in promtool and in
# vmalert-tool against the same inputs. Needs docker and python3; no network
# once the images are present.
set -euo pipefail
REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
# Apache-2.0, referenced by digest, used only by this check; tags and
# digests in deploy/kubernetes/dependencies/versions.lock.yaml
# (prometheusTool, victoriaMetricsAlert, victoriaMetricsAlertTool).
PROMTOOL_IMAGE=${PROMTOOL_IMAGE:-prom/prometheus:v3.16.0@sha256:e04987641b57704f408e31f6373ba4138ecf27cb4a165d24aabb6c38fddd0f06}
VMALERT_IMAGE=${VMALERT_IMAGE:-victoriametrics/vmalert:v1.147.0@sha256:45ea994098b77d56904f3d5d7cb2771effad9ddefb1d07d4ffa0c1da4c3e4c5c}
VMALERT_TOOL_IMAGE=${VMALERT_TOOL_IMAGE:-victoriametrics/vmalert-tool:v1.147.0@sha256:b89411be0487fbf0d966047dd76657c18f23522c4e547ae1ac757d644c52335a}

RULES=(gpu-recording-rules.yaml gpu-rules.yaml gpu-alert-rules.yaml gpu-shadow-rules.yaml self-rules.yaml)

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
for f in "${RULES[@]}"; do
	cp "$REPO_ROOT/configs/vmalert/$f" "$work/$f"
done
python3 -I "$REPO_ROOT/hack/dashboard-rules.py" >"$work/dashboards.yaml"
# vmalert-tool names the expression tests metricsql_expr_test; the cases
# are the same.
sed 's/^\([[:space:]]*\)promql_expr_test:/\1metricsql_expr_test:/' \
	"$REPO_ROOT/configs/vmalert/gpu-promql-rules.test.yaml" >"$work/vmalert-tool.test.yaml"
cp "$REPO_ROOT/configs/vmalert/gpu-promql-rules.test.yaml" "$work/promtool.test.yaml"
# The images run as an unprivileged user (nobody in prom/prometheus) and
# mktemp -d makes the directory 0700 for the caller: let them read it.
chmod 755 "$work"
chmod a+r "$work"/*

files=("${RULES[@]}" dashboards.yaml)
vmalert_args=()
for f in "${files[@]}"; do
	vmalert_args+=("-rule=/rules/$f")
done

run() {
	docker run --rm --network none -v "$work:/rules:ro" -w /rules "$@"
}

echo "promtool check rules"
run --entrypoint promtool "$PROMTOOL_IMAGE" check rules "${files[@]}"
echo "vmalert -dryRun"
run "$VMALERT_IMAGE" -dryRun "${vmalert_args[@]}" >"$work/vmalert.log" 2>&1 || {
	cat "$work/vmalert.log" >&2
	exit 1
}
echo "promtool test rules"
run --entrypoint promtool "$PROMTOOL_IMAGE" test rules promtool.test.yaml
echo "vmalert-tool unittest"
run "$VMALERT_TOOL_IMAGE" unittest -disableAlertgroupLabel --files=/rules/vmalert-tool.test.yaml
