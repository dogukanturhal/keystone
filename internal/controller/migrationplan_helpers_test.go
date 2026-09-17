// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// stubClient lets us simulate Create + Get behaviour deterministically
// for race-window tests. Implements only the subset of client.Client
// that ensureMigrationPlan touches.
type stubClient struct {
	client.Client

	createReturns error
	createCalls   int

	getResults []error
	getCalls   int

	existingPlan *keystonev1alpha1.MigrationPlan
}

func (s *stubClient) Create(_ context.Context, obj client.Object, _ ...client.CreateOption) error {
	s.createCalls++
	if s.createReturns == nil {
		obj.SetUID("created-uid")
		obj.SetResourceVersion("100")
	}
	return s.createReturns
}

func (s *stubClient) Get(_ context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	s.getCalls++
	if s.getCalls <= len(s.getResults) {
		err := s.getResults[s.getCalls-1]
		if err != nil {
			return err
		}
	}
	if s.existingPlan != nil {
		mp := obj.(*keystonev1alpha1.MigrationPlan)
		*mp = *s.existingPlan
	}
	return nil
}

func (s *stubClient) Scheme() *runtime.Scheme { return runtime.NewScheme() }

func notFoundErr() error {
	return apierrors.NewNotFound(schema.GroupResource{
		Group:    "keystone.hexxlock.io",
		Resource: "migrationplans",
	}, "test-plan")
}

func alreadyExistsErr() error {
	return apierrors.NewAlreadyExists(schema.GroupResource{
		Group:    "keystone.hexxlock.io",
		Resource: "migrationplans",
	}, "test-plan")
}

func mkBundleAndSchema(t *testing.T) (*keystonev1alpha1.MigrationBundle, *keystonev1alpha1.DatabaseSchema) {
	t.Helper()
	return &keystonev1alpha1.MigrationBundle{
			ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns"},
			Spec:       keystonev1alpha1.MigrationBundleSpec{Version: "v1"},
		}, &keystonev1alpha1.DatabaseSchema{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
		}
}

// TestEnsurePlan_HappyPathNoReread — Create succeeds, plan struct is
// populated by the Create call. We MUST NOT then re-Get from the
// cache (the bug fixed in this MR).
func TestEnsurePlan_HappyPathNoReread(t *testing.T) {
	bundle, schemaObj := mkBundleAndSchema(t)
	c := &stubClient{
		getResults: []error{notFoundErr()},
	}
	plan, err := ensureMigrationPlan(context.Background(), c, bundle, schemaObj, nil, nil)
	if err != nil {
		t.Fatalf("ensure plan: %v", err)
	}
	if plan.UID != "created-uid" {
		t.Errorf("Create did not populate plan.UID; got %q", plan.UID)
	}
	if c.createCalls != 1 {
		t.Errorf("Create calls = %d, want 1", c.createCalls)
	}
	if c.getCalls != 1 {
		t.Errorf("Get calls = %d, want 1 (only the dedup-by-name probe; the buggy code did 2)", c.getCalls)
	}
}

// TestEnsurePlan_AlreadyExistsPolls — when Create returns AlreadyExists,
// poll Get because the cache may not yet have the object that the
// apiserver-side commit confirmed exists.
func TestEnsurePlan_AlreadyExistsPolls(t *testing.T) {
	bundle, schemaObj := mkBundleAndSchema(t)
	existing := &keystonev1alpha1.MigrationPlan{
		ObjectMeta: metav1.ObjectMeta{Name: "b-v1-s-plan", Namespace: "ns", UID: "existing-uid"},
	}
	c := &stubClient{
		// 1: dedup-probe -> NotFound (so we Create)
		// 2-3: cache lag NotFound
		// 4: cache caught up -> nil (returns existingPlan)
		getResults:    []error{notFoundErr(), notFoundErr(), notFoundErr(), nil},
		existingPlan:  existing,
		createReturns: alreadyExistsErr(),
	}
	plan, err := ensureMigrationPlan(context.Background(), c, bundle, schemaObj, nil, nil)
	if err != nil {
		t.Fatalf("ensure plan polled to error: %v", err)
	}
	if plan.UID != "existing-uid" {
		t.Errorf("plan.UID = %q, want existing-uid", plan.UID)
	}
	if c.getCalls < 2 {
		t.Errorf("expected at least 2 Get calls (dedup + at least one poll); got %d", c.getCalls)
	}
}

// TestEnsurePlan_AlreadyExistsPollTimeout — if the cache never catches
// up, the poll must time out and surface a meaningful error referencing
// the offending plan name (so operators can grep their logs).
func TestEnsurePlan_AlreadyExistsPollTimeout(t *testing.T) {
	bundle, schemaObj := mkBundleAndSchema(t)
	c := &stubClient{
		getResults:    []error{notFoundErr()},
		createReturns: alreadyExistsErr(),
		existingPlan:  nil, // every Get past index 0 returns nil-result + nil plan = no-op + we re-loop
	}
	// Override Get to ALWAYS return NotFound after the dedup-probe so
	// the poll can never succeed.
	c.getResults = []error{notFoundErr()}
	c.existingPlan = nil

	// Use a real-time-bounded poll: tighten via a 250ms ctx deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	// Patch stub: keep returning NotFound forever for poll Gets.
	// The current stub returns nil after exhausting getResults; emulate
	// "always NotFound" via overriding via a wrapper.
	always := &alwaysNotFoundClient{stubClient: c}
	_, err := ensureMigrationPlan(ctx, always, bundle, schemaObj, nil, nil)
	if err == nil {
		t.Fatalf("expected timeout error after poll deadline; got nil")
	}
	if !strings.Contains(err.Error(), "b-v1-s-plan") {
		t.Errorf("error message does not name the plan: %v", err)
	}
}

// TestEnsurePlan_CreateNonAlreadyExistsErrorPropagates — a hard Create
// failure must NOT be swallowed by the AlreadyExists polling path.
func TestEnsurePlan_CreateNonAlreadyExistsErrorPropagates(t *testing.T) {
	bundle, schemaObj := mkBundleAndSchema(t)
	hardErr := errors.New("apiserver kaboom")
	c := &stubClient{
		getResults:    []error{notFoundErr()},
		createReturns: hardErr,
	}
	_, err := ensureMigrationPlan(context.Background(), c, bundle, schemaObj, nil, nil)
	if err == nil {
		t.Fatalf("expected hard error to propagate; got nil")
	}
	if !strings.Contains(err.Error(), "apiserver kaboom") {
		t.Errorf("error did not wrap the create cause: %v", err)
	}
	if c.getCalls != 1 {
		t.Errorf("Get calls = %d, want 1 (no poll on non-AlreadyExists Create error)", c.getCalls)
	}
}

// alwaysNotFoundClient overrides Get to return NotFound on every call
// after the first; used by the poll-timeout test to keep the cache
// "behind" indefinitely.
type alwaysNotFoundClient struct {
	*stubClient
}

func (a *alwaysNotFoundClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	a.getCalls++
	return notFoundErr()
}
