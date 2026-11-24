// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/controller"
	"github.com/kubeneuron/kubeneuron/internal/httpapi"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestFollowerShowsOwnerSettlementCommand(t *testing.T) {
	ctx := t.Context()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	at := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	target := types.Target{Node: "worker"}
	owner := &types.Incident{ID: "inc-owner", State: types.StateResolved, Target: target, CloudIdentity: &cloud.NodeIdentity{ProviderID: "aws:///zone/i-old"}, Successor: &types.SuccessorRecord{Outcome: types.SuccessorHandedOver, Successor: cloud.Successor{Operation: "op-1", Pool: "training", RequestedAt: at}}}
	follower := &types.Incident{ID: "inc-follower", State: types.StateEvaluating, Target: target, Class: types.ClassFellOffBus}
	err = st.WithTx(ctx, func(tx store.Tx) error {
		for _, inc := range []*types.Incident{owner, follower} {
			if err := tx.CreateIncident(ctx, inc); err != nil {
				return err
			}
			if _, err := tx.ClaimNodeRemediation(ctx, "worker", "uid", inc.ID, at); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c := controller.New(st, st, nil, nil, nil, nil, nil, nil, slog.Default())
	api := httpapi.New(nil)
	api.EnableOperatorAPI(c, "test-token")
	server := httptest.NewServer(api.Routes())
	defer server.Close()
	show := func() string {
		t.Helper()
		out, err := runCommand(t, server.URL, "incidents", "show", follower.ID)
		if err != nil {
			t.Fatal(err)
		}
		header, _, _ := strings.Cut(out, "audit trail:")
		return header
	}
	want, err := os.ReadFile("testdata/cloud-settlement.golden")
	if err != nil {
		t.Fatal(err)
	}
	_, hint, _ := strings.Cut(string(want), "\n")
	if got := show(); !strings.Contains(got, hint) {
		t.Fatalf("follower header missing owner's command:\n%s", got)
	}
	if _, err := runCommand(t, server.URL, "resolve", owner.ID, "--cloud-operation-settled", "--reason", "checked group activity"); err != nil {
		t.Fatal(err)
	}
	if got := show(); strings.Contains(got, "--cloud-operation-settled") || strings.Contains(got, "unavailable [node-repair]") {
		t.Fatalf("stale hold after settlement:\n%s", got)
	}
	_, err = runCommand(t, server.URL, "resolve", owner.ID, "--cloud-operation-settled", "--reason", "checked again")
	if err == nil || !strings.Contains(err.Error(), "Incident "+owner.ID+" has no open cloud operation to mark as settled.") {
		t.Fatalf("missing no-open-operation explanation: %v", err)
	}
}

func TestResolveEarlierSuccessorReportsContinuingAttempt(t *testing.T) {
	ctx := t.Context()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	inc := &types.Incident{
		ID: "inc-earlier", Attempt: 2, State: types.StateExecuting,
		Target: types.Target{Node: "worker"},
		Successor: &types.SuccessorRecord{
			Outcome:   types.SuccessorHandedOver,
			Successor: cloud.Successor{Operation: "inc-earlier-a1", Pool: "training"},
		},
	}
	if err := st.WithTx(ctx, func(tx store.Tx) error { return tx.CreateIncident(ctx, inc) }); err != nil {
		t.Fatal(err)
	}
	c := controller.New(st, st, nil, nil, nil, nil, nil, nil, slog.Default())
	api := httpapi.New(nil)
	api.EnableOperatorAPI(c, "test-token")
	server := httptest.NewServer(api.Routes())
	defer server.Close()
	got, err := runCommand(t, server.URL, "resolve", inc.ID, "--cloud-operation-settled", "--reason", "checked training pool")
	if err != nil {
		t.Fatal(err)
	}
	want := "Recorded: the replacement node left by an earlier attempt of incident inc-earlier is settled. The incident stays open; its current attempt continues."
	if strings.TrimSpace(got) != want {
		t.Fatalf("resolve output = %q, want %q", got, want)
	}
}
