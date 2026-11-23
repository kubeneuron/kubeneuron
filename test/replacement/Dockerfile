# syntax=docker/dockerfile:1
# SPDX-License-Identifier: Apache-2.0
FROM --platform=$BUILDPLATFORM golang:1.26.11@sha256:4165ee6bf3b7811fd7b40373290405c83d0167dcb87ee5f29c427cdcef45ff8f AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
      -overlay=test/replacement/overlay.json -trimpath -ldflags="-s -w" \
      -o /out/controller ./cmd/kubeneuron-controller

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG REHEARSAL_FIRST_NODE
ARG REHEARSAL_DELETE_SNAPSHOT
ARG REHEARSAL_REUSE_MACHINE
ARG REHEARSAL_LOSE
ENV REHEARSAL_DELETE_SNAPSHOT=$REHEARSAL_DELETE_SNAPSHOT REHEARSAL_REUSE_MACHINE=$REHEARSAL_REUSE_MACHINE REHEARSAL_LOSE=$REHEARSAL_LOSE
ENV REHEARSAL_FIRST_NODE=$REHEARSAL_FIRST_NODE
COPY --from=build /out/controller /kubeneuron-controller
ENTRYPOINT ["/kubeneuron-controller"]
