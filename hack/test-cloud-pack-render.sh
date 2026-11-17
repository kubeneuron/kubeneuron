#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
cd "$(dirname "$0")/.."
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Exercise the standalone renderer without running any installer mutations.
sed -n '/^render_cloud_pack() {$/,/^}$/p' deploy/install.sh > "$tmp/render.sh"
source "$tmp/render.sh"
NAME=fleet
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
