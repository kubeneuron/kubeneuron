# syntax=docker/dockerfile:1
# SPDX-License-Identifier: Apache-2.0

# Production multi-target build for the four KubeNeuron binaries.
# TARGETOS/TARGETARCH are set by buildx for multi-arch builds and default to
# the native platform under the classic builder (empty GOOS/GOARCH).
# BUILDPLATFORM is a buildx built-in, but `make docker` uses the classic
# builder, where it expands to an empty string and the build dies parsing
# the platform. Declaring it before FROM gives the classic builder a
# default while buildx still supplies the real value — so the released
# multi-arch image cross-compiles instead of emulating, and a plain local
# build keeps working.
ARG BUILDPLATFORM=linux/amd64
FROM --platform=$BUILDPLATFORM golang:1.26.11 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
# The adapter contract modules are wired in through replace directives;
# go mod download needs their go.mod files before the full copy.
COPY pkg/accelerator/go.mod pkg/accelerator/
COPY pkg/cloud/go.mod pkg/cloud/
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
      -o /out/kubeneuron-operator ./cmd/kubeneuron-operator \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
      -o /out/kubeneuron-controller ./cmd/kubeneuron-controller \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
      -o /out/kubeneuron-agent ./cmd/kubeneuron-agent \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
      -o /out/kubeneuronctl ./cmd/kubeneuronctl

FROM gcr.io/distroless/static-debian12:nonroot AS operator
LABEL org.opencontainers.image.source="https://github.com/kubeneuron/kubeneuron" \
      org.opencontainers.image.description="KubeNeuron operator" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/kubeneuron-operator /kubeneuron-operator
COPY NOTICE THIRD_PARTY_LICENSES LICENSE /licenses/
ENTRYPOINT ["/kubeneuron-operator"]

FROM gcr.io/distroless/static-debian12:nonroot AS controller
LABEL org.opencontainers.image.source="https://github.com/kubeneuron/kubeneuron" \
      org.opencontainers.image.description="KubeNeuron controller" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/kubeneuron-controller /kubeneuron-controller
COPY NOTICE THIRD_PARTY_LICENSES LICENSE /licenses/
ENTRYPOINT ["/kubeneuron-controller"]

# The agent runs privileged with hostPID in the managed DaemonSet and must
# read /dev/kmsg, so it stays root; the runtime pod spec is the authority.
#
# busybox supplies one binary the agent cannot do without: nsenter. Rebooting
# the node means asking the host's init, and the host's init is only reachable
# through PID 1's namespaces — a distroless image has no way to get there, so
# the Reboot action failed with exit 127 on real hardware. busybox is static
# (uclibc), so it needs no loader in an image that has none.
FROM busybox:1.38.0-uclibc@sha256:b2e35951f72501f6a99f1bf45bcfe6d4795be08c518c2f97d0ed2fe776958a3b AS busybox
# Record the version of the actual binary being shipped, for each architecture.
RUN busybox --help > /busybox.help 2>&1 \
 && head -n 1 /busybox.help > /busybox.VERSION \
 && grep -Eq '^BusyBox v1\.38\.0 ' /busybox.VERSION \
 && version=$(sed -n 's/^BusyBox v\([0-9]*\.[0-9]*\.[0-9]*\).*/\1/p' /busybox.VERSION) \
 && printf 'Upstream source: https://busybox.net/downloads/busybox-%s.tar.bz2\nSee OFFER for corresponding source requests.\n' "$version" > /busybox.SOURCE \
 && rm /busybox.help

# DCGM from NVIDIA packages is not redistributable in this image. Operators
# may explicitly supply a compatible client through hostTooling.dcgmiPath.
FROM gcr.io/distroless/base-debian12 AS agent
LABEL org.opencontainers.image.source="https://github.com/kubeneuron/kubeneuron" \
      org.opencontainers.image.description="KubeNeuron node agent" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=busybox /bin/busybox /bin/nsenter
COPY build/licenses/busybox/ /licenses/native/busybox/
COPY --from=busybox /busybox.VERSION /licenses/native/busybox/VERSION
COPY --from=busybox /busybox.SOURCE /licenses/native/busybox/SOURCE
COPY build/licenses/native-SOURCES /licenses/native/SOURCES
COPY --from=build /out/kubeneuron-agent /kubeneuron-agent
COPY NOTICE THIRD_PARTY_LICENSES LICENSE /licenses/
ENTRYPOINT ["/kubeneuron-agent"]

FROM gcr.io/distroless/static-debian12:nonroot AS cli
LABEL org.opencontainers.image.source="https://github.com/kubeneuron/kubeneuron" \
      org.opencontainers.image.description="kubeneuronctl CLI" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/kubeneuronctl /kubeneuronctl
COPY NOTICE THIRD_PARTY_LICENSES LICENSE /licenses/
ENTRYPOINT ["/kubeneuronctl"]
