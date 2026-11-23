# syntax=docker/dockerfile:1
# SPDX-License-Identifier: Apache-2.0
# AGENT_IMAGE is built locally by run.sh.
ARG AGENT_IMAGE
FROM ${AGENT_IMAGE}
COPY nvidia-smi /usr/bin/nvidia-smi
