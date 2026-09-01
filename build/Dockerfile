# syntax=docker/dockerfile:1
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
FROM --platform=$BUILDPLATFORM golang:1.27.0 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
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
ENTRYPOINT ["/kubeneuron-operator"]

FROM gcr.io/distroless/static-debian12:nonroot AS controller
LABEL org.opencontainers.image.source="https://github.com/kubeneuron/kubeneuron" \
      org.opencontainers.image.description="KubeNeuron controller" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/kubeneuron-controller /kubeneuron-controller
ENTRYPOINT ["/kubeneuron-controller"]

# The agent runs privileged with hostPID in the managed DaemonSet and must
# read /dev/kmsg, so it stays root; the runtime pod spec is the authority.
#
# busybox supplies one binary the agent cannot do without: nsenter. Rebooting
# the node means asking the host's init, and the host's init is only reachable
# through PID 1's namespaces — a distroless image has no way to get there, so
# the Reboot action failed with exit 127 on real hardware. busybox is static
# (uclibc), so it needs no loader in an image that has none.
FROM busybox:uclibc AS busybox

# The DCGM client. With the NVIDIA GPU Operator installed, dcgmi lives inside
# NVIDIA's own container and is absent from the node, so mounting host binaries
# cannot find it and the agent's runtime attestation stays degraded. The client
# is small (one shared library) and speaks to whichever host engine
# --nvidia-dcgm-endpoint names, so the agent carries its own.
#
# The version is pinned: it is what the agent attests as the DCGM runtime, so it
# must not drift silently between builds.
#
# It must also be NO NEWER than the host engine it will talk to. DCGM tolerates
# an engine newer than the client and not the reverse: a 4.6 client against a
# 4.3 engine connects, lists the GPU, and then fails every field read with
# "Return -20: The requested function was not found". Measured on a real T4,
# not inferred — and the previous pin of 4.6.1 was newer than the engine in
# EVERY NVIDIA GPU Operator release: v25.3 ships 4.3.1, v25.10 ships 4.4.x,
# v26.3 ships 4.5.2. The bundled client could therefore not speak to the engine
# in the deployment the documentation recommends, on any version, and the agent
# silently served the narrower nvidia-smi source instead.
#
# So pin to the OLDEST engine we support rather than the newest client
# available: 4.3.1 works against 4.3.1 and everything above it.
FROM debian:13-slim AS dcgm
ARG TARGETARCH
ARG DCGM_VERSION=4.3.1-1
RUN set -eux; \
    apt-get update -qq && apt-get install -y -qq --no-install-recommends curl ca-certificates; \
    arch="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    case "$arch" in \
      amd64) repo=x86_64; pkgarch=amd64 ;; \
      arm64) repo=sbsa;   pkgarch=arm64 ;; \
      *) echo "unsupported architecture $arch for the DCGM client" >&2; exit 1 ;; \
    esac; \
    url="https://developer.download.nvidia.com/compute/cuda/repos/debian12/${repo}/datacenter-gpu-manager-4-core_${DCGM_VERSION}_${pkgarch}.deb"; \
    curl -fsSL "$url" -o /tmp/dcgm.deb; \
    dpkg-deb -x /tmp/dcgm.deb /dcgm; \
    test -x /dcgm/usr/bin/dcgmi; \
    # distroless/base carries glibc but not libgcc_s, which dcgmi links against.
    cp -a /lib/*-linux-gnu*/libgcc_s.so.1 /dcgm/usr/lib/

FROM gcr.io/distroless/base-debian12 AS agent
LABEL org.opencontainers.image.source="https://github.com/kubeneuron/kubeneuron" \
      org.opencontainers.image.description="KubeNeuron node agent" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=busybox /bin/busybox /bin/nsenter
COPY --from=dcgm /dcgm/usr/bin/dcgmi /usr/bin/dcgmi
COPY --from=dcgm /dcgm/usr/lib/ /usr/lib/
COPY --from=build /out/kubeneuron-agent /kubeneuron-agent
ENTRYPOINT ["/kubeneuron-agent"]

FROM gcr.io/distroless/static-debian12:nonroot AS cli
LABEL org.opencontainers.image.source="https://github.com/kubeneuron/kubeneuron" \
      org.opencontainers.image.description="kubeneuronctl CLI" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/kubeneuronctl /kubeneuronctl
ENTRYPOINT ["/kubeneuronctl"]
