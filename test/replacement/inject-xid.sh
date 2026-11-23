#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
node=${1:?expected a kind worker container}
pci=${2:?expected a PCI address without the function}
[[ $pci =~ ^[[:xdigit:]]{4}:[[:xdigit:]]{2}:[[:xdigit:]]{2}$ ]]
[[ $(docker inspect -f '{{index .Config.Labels "io.x-k8s.kind.role"}}' "$node") == worker ]]
# Import into this worker's journal only. All kind nodes on the host share /dev/kmsg.
docker exec -i "$node" sh -es -- "$pci" <<'NODE'
pci=$1
writer=/usr/lib/systemd/systemd-journal-remote
test -x "$writer"
dir=/run/log/journal/replacement-test
mkdir -p "$dir"
line="NVRM: Xid (PCI:$pci): 79, pid=1, GPU has fallen off the bus."
printf '__REALTIME_TIMESTAMP=%s\n_BOOT_ID=%s\n_TRANSPORT=kernel\nSYSLOG_IDENTIFIER=kernel\nMESSAGE=%s\n\n' \
  "$(date +%s%6N)" "$(tr -d - </proc/sys/kernel/random/boot_id)" "$line" |
  "$writer" --output="$dir/system.journal" -
test "$(journalctl --file="$dir/system.journal" -n 1 -o cat --no-pager)" = "$line"
NODE
