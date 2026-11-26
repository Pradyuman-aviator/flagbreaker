// Package v1alpha1 contains the FlagGuard API.
// +kubebuilder:object:generate=true
// +groupName=flagbreaker.dev
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the API group and version of FlagGuard.
	GroupVersion = schema.GroupVersion{Group: "flagbreaker.dev", Version: "v1alpha1"}

	// SchemeBuilder registers the types in this package with a scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this package to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
