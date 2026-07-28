# syntax=docker/dockerfile:1
# Production multi-target build for the four KubeNeuron binaries.
# TARGETOS/TARGETARCH are set by buildx for multi-arch builds and default to
# the native platform under the classic builder (empty GOOS/GOARCH).
FROM golang:1.25.12 AS build
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
LABEL org.opencontainers.image.source="https://github.com/andreypetruk/kube-neuron" \
      org.opencontainers.image.description="KubeNeuron operator" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/kubeneuron-operator /kubeneuron-operator
ENTRYPOINT ["/kubeneuron-operator"]

FROM gcr.io/distroless/static-debian12:nonroot AS controller
LABEL org.opencontainers.image.source="https://github.com/andreypetruk/kube-neuron" \
      org.opencontainers.image.description="KubeNeuron controller" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/kubeneuron-controller /kubeneuron-controller
ENTRYPOINT ["/kubeneuron-controller"]

# The agent runs privileged with hostPID in the managed DaemonSet and must
# read /dev/kmsg, so it stays root; the runtime pod spec is the authority.
FROM gcr.io/distroless/static-debian12 AS agent
LABEL org.opencontainers.image.source="https://github.com/andreypetruk/kube-neuron" \
      org.opencontainers.image.description="KubeNeuron node agent" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/kubeneuron-agent /kubeneuron-agent
ENTRYPOINT ["/kubeneuron-agent"]

FROM gcr.io/distroless/static-debian12:nonroot AS cli
LABEL org.opencontainers.image.source="https://github.com/andreypetruk/kube-neuron" \
      org.opencontainers.image.description="kubeneuronctl CLI" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/kubeneuronctl /kubeneuronctl
ENTRYPOINT ["/kubeneuronctl"]
