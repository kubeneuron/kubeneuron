// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

type trainingOperator struct {
	*classOperator
	workers []types.TrainingWorker
}

func (o *trainingOperator) IncidentTrainingWorkers(context.Context, string) ([]types.TrainingWorker, error) {
	return o.workers, nil
}

func TestIncidentDetailDistinguishesUnavailableAndEmptyJobMap(t *testing.T) {
	op := &trainingOperator{classOperator: &classOperator{fakeOperator: &fakeOperator{}, class: types.ClassAgentDown}}
	handler := operatorServer(op, "secret")
	for _, tc := range []struct {
		workers []types.TrainingWorker
		want    string
	}{{nil, "null"}, {[]types.TrainingWorker{}, "[]"}} {
		op.workers = tc.workers
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/incidents/i1", "secret", ""))
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if rec.Code != 200 || string(body["affectedJobs"]) != tc.want || len(body["observe_only"]) == 0 {
			t.Fatalf("unexpected detail %s", rec.Body)
		}
	}
}
