# syntax=docker/dockerfile:1
# SPDX-License-Identifier: Apache-2.0

# Production multi-target build for the four KubeNeuron binaries.
# BuildKit supplies BUILDPLATFORM and TARGETOS/TARGETARCH automatically.
# Keep the compiler native to the builder and cross-compile the target binaries.
# Do not declare a global BUILDPLATFORM default: it overrides BuildKit's value
# and can force an amd64 compiler image on an arm64 runner.
FROM --platform=$BUILDPLATFORM golang:1.26.11@sha256:4165ee6bf3b7811fd7b40373290405c83d0167dcb87ee5f29c427cdcef45ff8f AS build
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

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS operator
LABEL org.opencontainers.image.source="https://github.com/kubeneuron/kubeneuron" \
      org.opencontainers.image.description="KubeNeuron operator" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/kubeneuron-operator /kubeneuron-operator
COPY NOTICE THIRD_PARTY_LICENSES LICENSE /licenses/
COPY build/licenses/base-image-packages.txt /licenses/BASE-IMAGE-PACKAGES
ENTRYPOINT ["/kubeneuron-operator"]

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS controller
LABEL org.opencontainers.image.source="https://github.com/kubeneuron/kubeneuron" \
      org.opencontainers.image.description="KubeNeuron controller" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/kubeneuron-controller /kubeneuron-controller
COPY NOTICE THIRD_PARTY_LICENSES LICENSE /licenses/
COPY build/licenses/base-image-packages.txt /licenses/BASE-IMAGE-PACKAGES
ENTRYPOINT ["/kubeneuron-controller"]

# The agent runs privileged with hostPID in the managed DaemonSet and must
# read /dev/kmsg, so it stays root; the runtime pod spec is the authority.
#
# The agent needs one host tool it cannot do without: nsenter. Rebooting the
# node means asking the host's init, and the host's init is only reachable
# through PID 1's namespaces — a distroless image has no way to get there.
#
# The agent image no longer ships BusyBox. Its nsenter helper is toybox
# (0BSD), statically linked against musl (MIT). The Debian base image still
# provides glibc (LGPL) as before. nsenter is built here from a checksum-pinned
# source as a single static binary, so it needs no loader; the stage runs on
# the target platform, so each architecture compiles natively.
FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc AS nsenter
ARG TOYBOX_VERSION=0.8.13
ARG TOYBOX_SHA256=9d4c124d7d731a2db399f6278baa2b42c2e3511f610c6ad30cc3f1a52581334b
ARG TOYBOX_GITHUB_SHA256=fa84ca399c2a88c3d841db943052911355b817f8151cd6bfb9e0b12541e4828c
ARG MUSL_VERSION=1.2.5
ARG MUSL_SHA256=a9a118bbe84d8764da0ea0d28b3ab3fae8477fc7e4085d90102b8596fc7c75e4
RUN apk add --no-cache bash build-base curl linux-headers
WORKDIR /src
RUN <<'SH'
set -eu
fetch() {
    archive=$1
    url=$2
    digest=$3
    curl --retry 2 --retry-all-errors --retry-delay 2 --connect-timeout 15 --max-time 120 -fsSLo "$archive" "$url" &&
        echo "$digest  $archive" | sha256sum -c - &&
        printf '%s\nsha256 %s\n' "$url" "$digest" > "$archive.source"
}
fetch toybox.tar.gz "https://codeload.github.com/landley/toybox/tar.gz/refs/tags/${TOYBOX_VERSION}" "$TOYBOX_GITHUB_SHA256" ||
    fetch toybox.tar.gz "https://landley.net/toybox/downloads/toybox-${TOYBOX_VERSION}.tar.gz" "$TOYBOX_SHA256"
fetch musl.tar.gz "https://distfiles.alpinelinux.org/distfiles/v3.20/musl-${MUSL_VERSION}.tar.gz" "$MUSL_SHA256" ||
    fetch musl.tar.gz "https://musl.libc.org/releases/musl-${MUSL_VERSION}.tar.gz" "$MUSL_SHA256"
apk list -I musl | grep -q "^musl-${MUSL_VERSION}-"
tar xzf toybox.tar.gz
tar xzf musl.tar.gz "musl-${MUSL_VERSION}/COPYRIGHT"
SH
# Alpine's fortify headers do not compile toybox; the fortification is a
# build-time hardening of this one small binary, not a licence matter.
RUN cd "toybox-${TOYBOX_VERSION}" \
 && LDFLAGS=--static CFLAGS="-Os -U_FORTIFY_SOURCE" bash scripts/single.sh nsenter \
 && ! readelf -l nsenter | grep -q INTERP \
 && mkdir -p /out/licenses/toybox /out/licenses/musl \
 && install -m 0755 nsenter /out/nsenter \
 && cp LICENSE /out/licenses/toybox/LICENSE \
 && cp "/src/musl-${MUSL_VERSION}/COPYRIGHT" /out/licenses/musl/COPYRIGHT \
 && printf 'toybox %s\n' "${TOYBOX_VERSION}" > /out/licenses/toybox/VERSION \
 && cp /src/toybox.tar.gz.source /out/licenses/toybox/SOURCE \
 && printf 'musl %s (statically linked into nsenter)\n' "${MUSL_VERSION}" > /out/licenses/musl/VERSION \
 && cp /src/musl.tar.gz.source /out/licenses/musl/SOURCE

# DCGM from NVIDIA packages is not redistributable in this image. Operators
# may explicitly supply a compatible client through hostTooling.dcgmiPath.
FROM gcr.io/distroless/base-debian12@sha256:fabbf1c0c357a3d42550111351daed089b20a2c954df13ee2fcff60602515e84 AS agent
LABEL org.opencontainers.image.source="https://github.com/kubeneuron/kubeneuron" \
      org.opencontainers.image.description="KubeNeuron node agent" \
      org.opencontainers.image.licenses="Apache-2.0 AND 0BSD AND MIT"
COPY --from=nsenter /out/nsenter /bin/nsenter
COPY --from=nsenter /out/licenses/ /licenses/native/
COPY build/licenses/native-SOURCES /licenses/native/SOURCES
COPY --from=build /out/kubeneuron-agent /kubeneuron-agent
COPY NOTICE THIRD_PARTY_LICENSES LICENSE /licenses/
COPY build/licenses/base-image-packages.txt /licenses/BASE-IMAGE-PACKAGES
ENTRYPOINT ["/kubeneuron-agent"]

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS cli
LABEL org.opencontainers.image.source="https://github.com/kubeneuron/kubeneuron" \
      org.opencontainers.image.description="kubeneuronctl CLI" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/kubeneuronctl /kubeneuronctl
COPY NOTICE THIRD_PARTY_LICENSES LICENSE /licenses/
COPY build/licenses/base-image-packages.txt /licenses/BASE-IMAGE-PACKAGES
ENTRYPOINT ["/kubeneuronctl"]
