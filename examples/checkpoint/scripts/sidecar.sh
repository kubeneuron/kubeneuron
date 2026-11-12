#!/bin/sh
# SPDX-License-Identifier: Apache-2.0

# The checkpoint sidecar: reads KubeNeuron's annotations off this Pod through
# the downward API and turns them into two signals to the trainer, which
# shares the Pod's process namespace (shareProcessNamespace: true):
#
#   advisory  -> SIGUSR1  "checkpoint when convenient; a disruption may follow"
#   request   -> SIGUSR2  "checkpoint now; the deadline is on the Pod"
#
# After a request, the sidecar waits for the trainer to write its checkpoint
# marker and exit; the Pod leaving is the acknowledgement. It never patches
# anything and never asks for more time: there is nothing to ask.
set -eu

ANNOTATIONS=${ANNOTATIONS:-/etc/podinfo/annotations}
CKPT_DIR=${CKPT_DIR:-/ckpt}
POLL=${POLL:-2}

TRAINER_SCRIPT=${TRAINER_SCRIPT:-/scripts/trainer.sh}

trainer_pid() {
	# The trainer is the one other process in this Pod running TRAINER_SCRIPT.
	for pid in /proc/[0-9]*; do
		pid=${pid#/proc/}
		cmd=$(tr '\0' ' ' <"/proc/$pid/cmdline" 2>/dev/null || true)
		case "$cmd" in
		*"$TRAINER_SCRIPT"*) printf '%s' "$pid"; return 0 ;;
		esac
	done
	return 1
}

value() {
	# Downward API renders annotations as key="value" lines, one per line.
	# Keys hold slashes, hence the | delimiter.
	sed -n "s|^$1=\"\(.*\)\"\$|\1|p" "$ANNOTATIONS" 2>/dev/null | head -n1
}

advised=""
requested=""
while :; do
	incident_adv=$(value 'kubeneuron.io/checkpoint-advisory-incident')
	round=$(value 'kubeneuron.io/checkpoint-advisory-round')
	if [ -n "$incident_adv" ] && [ "$advised" != "$incident_adv/$round" ]; then
		advised="$incident_adv/$round"
		echo "sidecar: advisory for incident $incident_adv round $round (next: $(value 'kubeneuron.io/checkpoint-advisory-next-action')) - asking the trainer to checkpoint when convenient"
		if pid=$(trainer_pid); then kill -USR1 "$pid" || true; fi
	fi
	incident_req=$(value 'kubeneuron.io/checkpoint-incident')
	deadline=$(value 'kubeneuron.io/checkpoint-deadline-at')
	if [ -n "$incident_req" ] && [ -n "$deadline" ] && [ "$requested" != "$incident_req" ]; then
		requested="$incident_req"
		echo "sidecar: request for incident $incident_req, deadline $deadline - asking the trainer to checkpoint now"
		if pid=$(trainer_pid); then kill -USR2 "$pid" || true; fi
		# Wait for the marker, then let the trainer finish; the Pod exiting is
		# the acknowledgement KubeNeuron reads.
		while [ ! -f "$CKPT_DIR/checkpoint.done" ]; do
			sleep 1
			if ! trainer_pid >/dev/null; then break; fi
		done
		# Say only what was seen: the marker, or a trainer that left without it.
		if [ -f "$CKPT_DIR/checkpoint.done" ]; then
			echo "sidecar: checkpoint written; exiting so the Pod can acknowledge by leaving"
		else
			echo "sidecar: trainer left without writing the checkpoint marker; exiting so the Pod can acknowledge by leaving"
		fi
		exit 0
	fi
	# When the trainer has finished on its own, there is nothing left to do.
	if ! trainer_pid >/dev/null && [ -f "$CKPT_DIR/trainer.exited" ]; then
		exit 0
	fi
	sleep "$POLL"
done
