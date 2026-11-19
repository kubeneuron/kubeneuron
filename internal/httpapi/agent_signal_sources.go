// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func (s *Server) handleAgentSignalSources(w http.ResponseWriter, r *http.Request) {
	principal, ok := AgentPrincipalFromContext(r.Context())
	if !ok || principal.NodeUID == "" || principal.NodeName == "" {
		http.Error(w, "agent authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var rows []types.AgentSignalSource
	if err := dec.Decode(&rows); err != nil {
		writeJSONDecodeError(w, "bad source report", err)
		return
	}
	if err := ensureJSONEOF(dec); err != nil {
		writeJSONDecodeError(w, "bad source report", err)
		return
	}
	if len(rows) > 64 || rows == nil {
		http.Error(w, "invalid source list", http.StatusBadRequest)
		return
	}
	seen := make(map[[2]string]bool, len(rows))
	for _, row := range rows {
		if row.Node != principal.NodeName {
			http.Error(w, "agent is not authorized for the requested node", http.StatusForbidden)
			return
		}
		key := [2]string{row.Source, string(row.Class)}
		if seen[key] || !validAgentSource(row) {
			http.Error(w, "invalid source observation", http.StatusBadRequest)
			return
		}
		seen[key] = true
	}
	backend, ok := s.backend.(interface {
		RecordAgentSignalSources(string, []types.AgentSignalSource)
	})
	if !ok {
		http.Error(w, "source reports unavailable", http.StatusServiceUnavailable)
		return
	}
	backend.RecordAgentSignalSources(principal.NodeName, rows)
	w.WriteHeader(http.StatusNoContent)
}

func validAgentSource(row types.AgentSignalSource) bool {
	reason := "kernel-log-unavailable"
	switch row.Source {
	case "amd-kmsg":
		if row.Class != types.ClassDriverHang && row.Class != types.ClassPCIe {
			return false
		}
	case "nvidia-kmsg", "nvidia-dcgm":
		switch row.Class {
		case types.ClassXIDApp, types.ClassECCDBE, types.ClassECCSBERate, types.ClassECCContained, types.ClassFellOffBus, types.ClassGSPError, types.ClassNVLink, types.ClassRowRemapFailure, types.ClassRowRemapOK:
		default:
			return false
		}
		if row.Source == "nvidia-dcgm" {
			reason = "xid-field-unavailable"
		}
	default:
		return false
	}
	if row.Available {
		reason = ""
	}
	if row.Source != "nvidia-dcgm" && !row.Available && row.Reason == "journal-unreadable" {
		// An agent with a journal mount that it cannot read.
		reason = row.Reason
	}
	return !row.ReadAt.IsZero() && row.Reason == reason
}
