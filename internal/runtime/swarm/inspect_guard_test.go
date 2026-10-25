package swarm

import (
	"reflect"
	"strings"
	"testing"
)

// TestInspectAPIIsReadOnly is a guard for the read-only M0 contract: a diff
// must never be able to mutate the cluster, so the Docker surface used by
// Inspect may only contain list/inspect calls. Adding a mutating method here
// should fail this test.
func TestInspectAPIIsReadOnly(t *testing.T) {
	typ := reflect.TypeOf((*api)(nil)).Elem()
	if typ.NumMethod() == 0 {
		t.Fatal("api interface has no methods")
	}

	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		if !strings.HasSuffix(name, "List") && !strings.HasPrefix(name, "Inspect") {
			t.Errorf("api method %q is not a read-only list/inspect call", name)
		}
	}
}
