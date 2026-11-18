// SPDX-License-Identifier: Apache-2.0

package checkpoint

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// ReadTrainingReport decodes the bounded request body after caller authentication.
// A nonzero status is safe to return without including details from the request.
func ReadTrainingReport(w http.ResponseWriter, r *http.Request) (TrainingReport, int) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		return TrainingReport{}, http.StatusMethodNotAllowed
	}
	const maxReportBytes = 4096
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxReportBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return TrainingReport{}, http.StatusRequestEntityTooLarge
		}
		return TrainingReport{}, http.StatusBadRequest
	}
	// Inspect tokens to reject duplicate keys and case variants that struct
	// decoding would otherwise silently accept.
	fields := map[string]bool{}
	var report TrainingReport
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return TrainingReport{}, http.StatusBadRequest
	}
	for dec.More() {
		key, err := dec.Token()
		name, ok := key.(string)
		if err != nil || !ok || fields[name] {
			return TrainingReport{}, http.StatusBadRequest
		}
		fields[name] = true
		switch name {
		case "kind":
			err = dec.Decode(&report.Kind)
		case "checkpoint_id":
			err = dec.Decode(&report.CheckpointID)
		case "step":
			err = dec.Decode(&report.Step)
		default:
			return TrainingReport{}, http.StatusBadRequest
		}
		if err != nil || (name == "step" && report.Step == nil) || (name == "checkpoint_id" && report.CheckpointID == "") {
			return TrainingReport{}, http.StatusBadRequest
		}
	}
	if tok, err = dec.Token(); err != nil || tok != json.Delim('}') {
		return TrainingReport{}, http.StatusBadRequest
	}
	if _, err = dec.Token(); err != io.EOF || report.Validate() != nil {
		return TrainingReport{}, http.StatusBadRequest
	}
	return report, 0
}
