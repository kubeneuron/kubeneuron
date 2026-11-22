// SPDX-License-Identifier: Apache-2.0

package checkpoint

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadTrainingReport(t *testing.T) {
	for _, tc := range []struct {
		method, body string
		status       int
	}{
		{"POST", `{"kind":"progress","step":0}`, 0},
		{"GET", "", 405},
		{"POST", strings.Repeat(" ", 4097), 413},
		{"POST", `{"kind":"progress","step":0,"step":4}`, 400},
		{"POST", `{"kind":"progress","STEP":2}`, 400},
		{"POST", `{"kind":"progress","step":2,"job":"other"}`, 400},
		{"POST", `{"kind":"progress","step":2} {}`, 400},
		{"POST", `{"kind":"checkpoint","checkpoint_id":"x","step":null}`, 400},
	} {
		r := httptest.NewRequest(tc.method, "/", strings.NewReader(tc.body))
		_, status := ReadTrainingReport(httptest.NewRecorder(), r)
		if status != tc.status {
			t.Errorf("%s (%d bytes): status %d want %d", tc.method, len(tc.body), status, tc.status)
		}
	}
}
