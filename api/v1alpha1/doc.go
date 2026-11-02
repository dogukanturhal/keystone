// SPDX-License-Identifier: Apache-2.0
//
// Keystone — database lifecycle for tier-1 multi-tenant SaaS.
// Copyright 2026 HexxLock
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package v1alpha1 contains the first iteration of the Keystone API surface.
//
// Stability contract for v1alpha1: any field marked with the
// `+keystone:stability=alpha` marker may change between minor releases without
// a deprecation cycle. Fields without that marker are intended to graduate to
// v1beta1 unchanged. New fields land here first.
//
// +kubebuilder:object:generate=true
// +groupName=keystone.hexxlock.io
package v1alpha1
