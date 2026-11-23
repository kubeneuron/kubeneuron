#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Runs the rehearsal's audit assertions against recorded incidents: each
# pass-* fixture must satisfy its assertion, each fail-* fixture must not.
# No cluster; a second.
set -euo pipefail
cd "$(dirname "$0")"
failed=0
for f in testdata/early-hold/*/*.json; do
  name=$(basename "$f" .json)
  revision=$(basename "$(dirname "$f")")
  rc=0
  jq -e --arg before sha256:before --arg after sha256:after --arg revision "$revision" -f assert-early-hold.jq "$f" >/dev/null || rc=$?
  case $rc in 0) got=pass ;; 1) got=fail ;; *) got="jq error $rc" ;; esac
  if [[ $name != "$got"-* ]]; then
    echo "assert-early-hold.jq: $revision/$name: got $got" >&2
    failed=1
  fi
done
for f in testdata/same-zone/*.json; do
  rc=0
  jq -e -f assert-same-zone.jq "$f" >/dev/null || rc=$?
  case "$(basename "$f"):$rc" in
    pass-*:0|fail-*:1) ;;
    *) echo "assert-same-zone.jq: $f returned $rc" >&2; failed=1 ;;
  esac
done
for f in testdata/any-zone/*.json; do
  rc=0
  jq -e --arg node successor -f assert-any-zone.jq "$f" >/dev/null || rc=$?
  case "$(basename "$f"):$rc" in
    pass-*:0|fail-*:1) ;;
    *) echo "assert-any-zone.jq: $f returned $rc" >&2; failed=1 ;;
  esac
done
exit "$failed"
