// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/kubeneuron/kubeneuron/api/v1alpha1"
)

type uninstallReader struct {
	client.Reader
	name string
	err  error
}

func (r uninstallReader) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	if r.name != "" {
		var root api.KubeNeuron
		root.Name = r.name
		list.(*api.KubeNeuronList).Items = []api.KubeNeuron{root}
	}
	return r.err
}

func TestCheckRemainingInstallations(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reader uninstallReader
		want   string
	}{
		{"empty", uninstallReader{}, ""},
		{"removed CRD", uninstallReader{err: &meta.NoKindMatchError{}}, ""},
		{"remaining root", uninstallReader{name: "fleet"}, "fleet"},
		{"access denied", uninstallReader{err: errors.New("forbidden")}, "forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, err := remainingInstallation(t.Context(), tc.reader)
			if tc.reader.err == nil || meta.IsNoMatchError(tc.reader.err) {
				if err != nil || name != tc.want {
					t.Fatalf("got %q, %v; want %q", name, err, tc.want)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}
