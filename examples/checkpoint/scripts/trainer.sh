#!/bin/sh
# A CPU-only stand-in for a training loop: one "step" every second, a
# checkpoint every CHECKPOINT_EVERY steps, and two signals from the sidecar:
#
#   SIGUSR1  advisory - checkpoint at the next step boundary and keep going
#   SIGUSR2  request  - checkpoint now and exit 75 (EX_TEMPFAIL): the Pod
#                       leaving is the acknowledgement, and a non-zero exit
#                       lets the Job replace the index instead of completing it
#
# The checkpoint is a copy of the step counter into CKPT_DIR; a real trainer
# would write its model state the same way it already does.
set -u

CKPT_DIR=${CKPT_DIR:-/ckpt}
CHECKPOINT_EVERY=${CHECKPOINT_EVERY:-30}
STEPS=${STEPS:-3600} # an hour of one-second steps; the sidecar or a deletion ends it earlier
mkdir -p "$CKPT_DIR"

step=0
want_checkpoint=0
want_exit=0
why=periodic
trap 'want_checkpoint=1; why=advisory' USR1
trap 'want_checkpoint=1; want_exit=1; why=request' USR2

checkpoint() {
	# The line names WHY this checkpoint was taken, so a log can tell a
	# periodic one from one the advisory or the request asked for.
	printf '%s\n' "$step" >"$CKPT_DIR/step"
	echo "trainer: checkpoint at step $step ($why)"
	want_checkpoint=0
	why=periodic
}

if [ -f "$CKPT_DIR/step" ]; then
	step=$(cat "$CKPT_DIR/step")
	echo "trainer: resuming from step $step"
fi
while [ "$step" -lt "$STEPS" ]; do
	step=$((step + 1))
	sleep 1 &
	wait $! || true # a signal interrupts the sleep, not the loop
	if [ "$want_checkpoint" = 1 ] || [ $((step % CHECKPOINT_EVERY)) = 0 ]; then
		checkpoint
	fi
	if [ "$want_exit" = 1 ]; then
		touch "$CKPT_DIR/checkpoint.done"
		echo "trainer: exiting after the requested checkpoint"
		touch "$CKPT_DIR/trainer.exited"
		exit "${PREEMPT_EXIT_CODE:-75}"
	fi
done
checkpoint
touch "$CKPT_DIR/trainer.exited"
echo "trainer: done"
