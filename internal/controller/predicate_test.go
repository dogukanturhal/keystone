// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// TestIgnoreStatusOnlyUpdates_StatusOnly is the canonical regression
// test for the 2026-05-04 OOM cascade: a status-only patch must NOT
// pass the filter, otherwise the controller re-reconciles and patches
// status again, hot-looping.
func TestIgnoreStatusOnlyUpdates_StatusOnly(t *testing.T) {
	pred := ignoreStatusOnlyUpdates()
	old := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "x",
			Namespace:   "y",
			Generation:  1,
			Labels:      map[string]string{"a": "1"},
			Annotations: map[string]string{"b": "2"},
		},
	}
	// Same metadata (only ResourceVersion would differ in a status-only
	// patch — which we don't compare).
	newer := old.DeepCopy()
	newer.ResourceVersion = "999"

	if pred.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: newer}) {
		t.Fatal("status-only update must NOT pass the filter")
	}
}

func TestIgnoreStatusOnlyUpdates_GenerationChanged(t *testing.T) {
	pred := ignoreStatusOnlyUpdates()
	old := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "x", Namespace: "y", Generation: 1,
		},
	}
	newer := old.DeepCopy()
	newer.Generation = 2

	if !pred.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: newer}) {
		t.Fatal("generation change MUST pass the filter (spec changed)")
	}
}

func TestIgnoreStatusOnlyUpdates_LabelsChanged(t *testing.T) {
	pred := ignoreStatusOnlyUpdates()
	old := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "x", Namespace: "y",
			Labels: map[string]string{"a": "1"},
		},
	}
	newer := old.DeepCopy()
	newer.Labels = map[string]string{"a": "2"}

	if !pred.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: newer}) {
		t.Fatal("label change MUST pass the filter")
	}
}

func TestIgnoreStatusOnlyUpdates_AnnotationsChanged(t *testing.T) {
	pred := ignoreStatusOnlyUpdates()
	old := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "x", Namespace: "y",
			Annotations: map[string]string{"a": "1"},
		},
	}
	newer := old.DeepCopy()
	newer.Annotations = map[string]string{"a": "2"}

	if !pred.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: newer}) {
		t.Fatal("annotation change MUST pass the filter (e.g. approval annotations)")
	}
}

func TestIgnoreStatusOnlyUpdates_FinalizersChanged(t *testing.T) {
	pred := ignoreStatusOnlyUpdates()
	old := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "x", Namespace: "y",
			Finalizers: []string{"keystone.hexxlock.io/foo"},
		},
	}
	newer := old.DeepCopy()
	newer.Finalizers = nil

	if !pred.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: newer}) {
		t.Fatal("finalizer change MUST pass the filter (cleanup needed)")
	}
}

func TestIgnoreStatusOnlyUpdates_DeletionTimestampSet(t *testing.T) {
	pred := ignoreStatusOnlyUpdates()
	old := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "x", Namespace: "y",
		},
	}
	newer := old.DeepCopy()
	now := metav1.NewTime(time.Now())
	newer.DeletionTimestamp = &now

	if !pred.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: newer}) {
		t.Fatal("deletionTimestamp set MUST pass the filter (deletion handler runs)")
	}
}

func TestIgnoreStatusOnlyUpdates_CreateAlwaysPasses(t *testing.T) {
	pred := ignoreStatusOnlyUpdates()
	o := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "x"}}
	if !pred.Create(event.CreateEvent{Object: o}) {
		t.Fatal("Create event must always pass")
	}
}

func TestIgnoreStatusOnlyUpdates_DeleteAlwaysPasses(t *testing.T) {
	pred := ignoreStatusOnlyUpdates()
	o := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "x"}}
	if !pred.Delete(event.DeleteEvent{Object: o}) {
		t.Fatal("Delete event must always pass")
	}
}

func TestIgnoreStatusOnlyUpdates_NilObjectsPass(t *testing.T) {
	pred := ignoreStatusOnlyUpdates()
	// Defensive: if either side is nil, let the reconciler handle it
	// (it will re-fetch from the cache and handle NotFound).
	if !pred.Update(event.UpdateEvent{ObjectOld: nil, ObjectNew: nil}) {
		t.Fatal("nil objects must pass through (defensive)")
	}
}

func TestMapsEqualStr(t *testing.T) {
	cases := []struct {
		a, b map[string]string
		want bool
	}{
		{nil, nil, true},
		{map[string]string{}, nil, true},
		{map[string]string{"a": "1"}, map[string]string{"a": "1"}, true},
		{map[string]string{"a": "1"}, map[string]string{"a": "2"}, false},
		{map[string]string{"a": "1"}, map[string]string{"b": "1"}, false},
		{map[string]string{"a": "1"}, map[string]string{"a": "1", "b": "2"}, false},
	}
	for i, c := range cases {
		if got := mapsEqualStr(c.a, c.b); got != c.want {
			t.Errorf("case %d: mapsEqualStr(%v, %v) = %v, want %v", i, c.a, c.b, got, c.want)
		}
	}
}

func TestSlicesEqualStr(t *testing.T) {
	cases := []struct {
		a, b []string
		want bool
	}{
		{nil, nil, true},
		{[]string{}, nil, true},
		{[]string{"a"}, []string{"a"}, true},
		{[]string{"a"}, []string{"b"}, false},
		{[]string{"a", "b"}, []string{"b", "a"}, false}, // order matters (finalizers are ordered)
	}
	for i, c := range cases {
		if got := slicesEqualStr(c.a, c.b); got != c.want {
			t.Errorf("case %d: slicesEqualStr(%v, %v) = %v, want %v", i, c.a, c.b, got, c.want)
		}
	}
}
