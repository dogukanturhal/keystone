// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is the group/version pair for the v1alpha1 API.
var GroupVersion = schema.GroupVersion{
	Group:   "keystone.hexxlock.io",
	Version: "v1alpha1",
}

// SchemeBuilder is used by code generation and the manager to register the
// v1alpha1 types with a runtime.Scheme.
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

// AddToScheme adds the types in this group to the given scheme. It is the
// canonical entrypoint used by the manager during startup.
var AddToScheme = SchemeBuilder.AddToScheme
