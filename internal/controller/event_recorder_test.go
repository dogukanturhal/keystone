// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
)

type capturedEventRecorder struct {
	*events.FakeRecorder
	action string
}

func (r *capturedEventRecorder) Eventf(regarding runtime.Object, related runtime.Object, eventtype, reason, action, note string, args ...interface{}) {
	r.action = action
	r.FakeRecorder.Eventf(regarding, related, eventtype, reason, action, note, args...)
}

func TestReconcileEventRecorder(t *testing.T) {
	fake := &capturedEventRecorder{FakeRecorder: events.NewFakeRecorder(1)}
	recorder := reconcileEventRecorder{fake}
	recorder.Eventf(&corev1.ConfigMap{}, corev1.EventTypeWarning, "ContentHashChanged", "version %s changed", "v1")
	select {
	case event := <-fake.Events:
		if fake.action != "Reconcile" || !strings.Contains(event, "Warning ContentHashChanged version v1 changed") {
			t.Fatalf("event lost type, reason, action or formatted note: %s", event)
		}
	default:
		t.Fatal("event was not emitted")
	}
}
