// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
)

// eventRecorder keeps the small API used by reconcilers and legacy fake
// recorders, while manager-backed events use the current events.k8s.io API.
type eventRecorder interface {
	Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{})
}

type reconcileEventRecorder struct{ events.EventRecorder }

func (r reconcileEventRecorder) Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	r.EventRecorder.Eventf(object, nil, eventtype, reason, "Reconcile", messageFmt, args...)
}

func eventRecorderFor(mgr ctrl.Manager, name string) eventRecorder {
	return reconcileEventRecorder{mgr.GetEventRecorder(name)}
}
