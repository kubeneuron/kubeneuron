FROM scratch AS operator
COPY bin/kubeneuron-operator /kubeneuron-operator
USER 65532:65532
ENTRYPOINT ["/kubeneuron-operator"]

FROM scratch AS controller
COPY bin/kubeneuron-controller /kubeneuron-controller
ENTRYPOINT ["/kubeneuron-controller"]

FROM scratch AS agent
COPY bin/kubeneuron-agent /kubeneuron-agent
ENTRYPOINT ["/kubeneuron-agent"]
