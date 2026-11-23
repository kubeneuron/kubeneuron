# Node replacement rehearsal

Run `make test-replacement-kind` with Docker, kind, kubectl, Go and jq.
The controller uses simulated AWS clients and the agent reports a synthetic Tesla T4.
Two journal faults exercise add-first and remove-first in one pool and workflow store.
Two more faults check policy qualification and configuration reload before drain.
Real AWS replacement, physical GPU health and CUDA execution require separate validation.
The runner builds the test images locally, loads them into kind and never publishes them.
