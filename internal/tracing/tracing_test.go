// SPDX-License-Identifier: AGPL-3.0-or-later

package tracing

import (
	"context"
	"testing"
)

// Regression: building the resource must not fail with "conflicting Schema URL".
func TestInit_WithEndpointBuildsResource(t *testing.T) {
	_, shutdown, err := Init(context.Background(), ProviderConfig{
		Endpoint:       "http://127.0.0.1:4318",
		ServiceVersion: "test",
		SampleRatio:    1.0,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Logf("shutdown (no collector reachable, ignored): %v", err)
	}
}

func TestInit_NoEndpointIsNoop(t *testing.T) {
	tp, shutdown, err := Init(context.Background(), ProviderConfig{})
	if err != nil || tp == nil {
		t.Fatalf("Init: tp=%v err=%v", tp, err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
