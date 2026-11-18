#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
cd "$(dirname "$0")/.."
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Exercise the standalone renderer without running any installer mutations.
sed -n '/^render_cloud_pack() {$/,/^}$/p' deploy/install.sh > "$tmp/render.sh"
# shellcheck source=/dev/null
source "$tmp/render.sh"
export NAME=fleet
render_cloud_pack | yq -o=json -I=0 '.' | jq -sS 'sort_by(.kind,.metadata.name)' > "$tmp/actual"

# Expand installation-owned names and labels as they appear in a fresh install.
kubectl kustomize config/cloud-policies |
  yq -o=json -I=0 '.' |
  jq -sS '
    def install_name: sub("^kubeneuron-cloud-observe$"; "fleet-observe") | sub("^kubeneuron-"; "fleet-");
    map(.metadata.name |= install_name |
        .spec.kubeNeuronRef = "fleet" |
        .metadata.labels += {"kubeneuron.io/installed-by":"install-sh","kubeneuron.io/installation":"fleet"} |
        if .spec.playbookRef then .spec.playbookRef |= install_name else . end |
        if .spec.onFailure.playbookRef then .spec.onFailure.playbookRef |= install_name else . end)
    | sort_by(.kind,.metadata.name)' > "$tmp/expected"
diff -u "$tmp/expected" "$tmp/actual"

# Check the action boundary independently of parity: both copies could acquire
# the same unsafe edit. Check all books, including those reached by escalation.
jq -e '
  [ .[] | select(.kind == "GPUPlaybook") ] as $books |
  [$books[].metadata.name] as $names |
  ["Observe", "Cordon", "Drain", "RecycleNode", "ReplaceNode"] as $allowed |
  ($books | length > 0) and
  all($books[].spec.steps[].action; . as $action | $allowed | index($action) != null) and
  all(.[] | (.spec.playbookRef, .spec.onFailure.playbookRef) | select(. != null);
      . as $ref | $names | index($ref) != null)
' "$tmp/actual" >/dev/null
