// SPDX-License-Identifier: Apache-2.0
//
// Keystone — database lifecycle for tier-1 multi-tenant SaaS.
// Copyright 2026 HexxLock
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

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
