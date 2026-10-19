# Fixture image for hack/kind-gpu-observability.sh. FROM scratch on purpose:
# the only content is the two statically linked binaries the harness builds
# on the host (hack/dcgmfixture, the synthetic exporter, and hack/alertsink,
# the webhook recorder), so no base image (and nothing unpinned) is pulled.
# CPU-only; nothing here touches a GPU, a driver or the controller.
FROM scratch
COPY dcgmfixture /dcgmfixture
COPY alertsink /alertsink
USER 65532:65532
ENTRYPOINT ["/dcgmfixture"]
