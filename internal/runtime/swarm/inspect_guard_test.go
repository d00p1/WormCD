package swarm

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestInspectAPIIsReadOnly is a guard for the read-only contract: Inspect may
// only use list/inspect calls, so a diff can never mutate the cluster. Adding a
// mutating method to readAPI should fail this test.
func TestInspectAPIIsReadOnly(t *testing.T) {
	typ := reflect.TypeOf((*readAPI)(nil)).Elem()
	if typ.NumMethod() == 0 {
		t.Fatal("readAPI has no methods")
	}

	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		if !strings.HasSuffix(name, "List") && !strings.HasSuffix(name, "Inspect") {
			t.Errorf("readAPI method %q is not a read-only list/inspect call", name)
		}
	}
}

// TestWriteAPIIsCreateOrRemove keeps the mutating surface to the calls Apply
// and Prune are allowed to make. The only update is ConfigUpdate, which changes
// labels in place. Secrets, networks and volumes have no in-place update.
func TestWriteAPIIsCreateOrRemove(t *testing.T) {
	typ := reflect.TypeOf((*writeAPI)(nil)).Elem()
	var updates []string
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		switch {
		case strings.HasSuffix(name, "Create"), strings.HasSuffix(name, "Remove"):
		case strings.HasSuffix(name, "Update"):
			updates = append(updates, name)
		default:
			t.Errorf("writeAPI method %q is not a create, remove or update call", name)
		}
	}
	if want := []string{"ConfigUpdate"}; !slices.Equal(updates, want) {
		t.Errorf("writeAPI updates = %v, want exactly %v", updates, want)
	}
}
