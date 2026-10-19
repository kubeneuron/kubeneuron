// Package depprofile holds the static tests for the external dependency
// profile under deploy/kubernetes/dependencies: the dcgm-exporter
// reliability profile in the GPU Operator values, the scrape label contract
// in the VMAgent resource, and the read-only RBAC that contract relies on.
//
// Nothing here runs in a cluster. The tests parse the shipped files the way
// their consumers do (Helm values as YAML, the exporter metrics list as CSV,
// the inline scrape config as Prometheus-style relabel rules) and pin them to
// the GPU metric catalog (internal/gpucatalog) and to the public documents
// that describe them, so none of the three can drift from the others in
// silence. The package has no non-test code of its own.
package depprofile
