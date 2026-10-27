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
		if !strings.HasSuffix(name, "List") && !strings.HasPrefix(name, "Inspect") {
			t.Errorf("readAPI method %q is not a read-only list/inspect call", name)
		}
	}
}

// TestWriteAPIIsCreateOrRemove keeps the mutating surface to the calls Apply
// and Prune are allowed to make.
//
// ConfigUpdate is the only update method on purpose. It is allowed because it
// changes only labels: config data is immutable in Docker, and a content change
// gets a new name instead. Secrets, networks and volumes must not get an update
// method; a new update method on writeAPI must be added to the list below
// deliberately, and this test fails until it is.
func TestWriteAPIIsCreateOrRemove(t *testing.T) {
	typ := reflect.TypeOf((*writeAPI)(nil)).Elem()
	var updates []string
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		switch {
		case strings.HasSuffix(name, "Create"), strings.HasSuffix(name, "Remove"):
		case strings.HasSuffix(name, "Update"):
			updates = append(updates, name)
		case name == "ConfigInspect":
			// Read-only: Apply reads the version it passes to ConfigUpdate.
		default:
			t.Errorf("writeAPI method %q is not a create, remove, update or inspect call", name)
		}
	}
	if want := []string{"ConfigUpdate"}; !slices.Equal(updates, want) {
		t.Errorf("writeAPI updates = %v, want exactly %v", updates, want)
	}
}
