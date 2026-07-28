// Package v1alpha1 contains the Kubernetes API for KubeNeuron.
//
// The API owns desired configuration only. Incident execution, leases, and
// audit records remain durable workflow state outside Kubernetes object status.
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion identifies the KubeNeuron API version.
	GroupVersion = schema.GroupVersion{Group: "kubeneuron.io", Version: "v1alpha1"}

	// SchemeBuilder registers KubeNeuron types with a Kubernetes scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds all KubeNeuron API types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
