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

	for i := range typ.NumMethod() {
		name := typ.Method(i).Name
		if !strings.HasSuffix(name, "List") && !strings.HasPrefix(name, "Inspect") {
			t.Errorf("readAPI method %q is not a read-only list/inspect call", name)
		}
	}
}

// writeAllowlist is every mutating call the runtime may make, with the reason.
// TestWriteAPIIsAllowlisted fails for a writeAPI method that is not listed here,
// so a new write needs a deliberate entry and a reason, and a removed method
// must leave the list too.
var writeAllowlist = map[string]string{
	"NetworkCreate": "Apply creates the networks of the plan",
	"NetworkRemove": "Prune removes managed networks that no service uses",
	"ConfigCreate":  "Apply creates the configs of the plan; a content change is a new name",
	"ConfigUpdate":  "changes labels only: Docker fixes a config's data, and the diff never asks for a data change",
	"ConfigRemove":  "Prune removes managed configs that no service uses",
	"SecretCreate":  "reserved: v0 has only external secrets, and preflight refuses a managed one before this call",
	"SecretRemove":  "Prune removes managed secrets that no service uses",
	"ServiceCreate": "Apply creates the services of the plan",
	"ServiceUpdate": "Apply replaces the spec of an existing service; the version comes from ServiceInspect at Apply time",
	"ServiceRemove": "Apply removes a service whose plan entry is Delete, after checking that it belongs to the target",
	"VolumeCreate":  "Apply creates the volumes of the plan",
	"VolumeRemove":  "reserved: volumes hold data and Prune never removes them",
}

// TestWriteAPIIsAllowlisted keeps the mutating surface to the calls listed in
// writeAllowlist.
func TestWriteAPIIsAllowlisted(t *testing.T) {
	typ := reflect.TypeOf((*writeAPI)(nil)).Elem()
	seen := make(map[string]bool, typ.NumMethod())
	for i := range typ.NumMethod() {
		name := typ.Method(i).Name
		seen[name] = true
		reason, ok := writeAllowlist[name]
		if !ok {
			t.Errorf("writeAPI method %q is not in writeAllowlist; list it with a reason or do not add it", name)
		} else if reason == "" {
			t.Errorf("writeAllowlist entry %q has no reason", name)
		}
	}
	for name := range writeAllowlist {
		if !seen[name] {
			t.Errorf("writeAllowlist entry %q is not a writeAPI method", name)
		}
	}
}

// TestWriteAPIUpdatesAreDeliberate pins the update methods. Only the config
// labels and the service spec may change in place; a network, a volume or a
// secret must not get an update method.
func TestWriteAPIUpdatesAreDeliberate(t *testing.T) {
	typ := reflect.TypeOf((*writeAPI)(nil)).Elem()
	var updates []string
	for i := range typ.NumMethod() {
		if name := typ.Method(i).Name; strings.HasSuffix(name, "Update") {
			updates = append(updates, name)
		}
	}
	slices.Sort(updates)
	if want := []string{"ConfigUpdate", "ServiceUpdate"}; !slices.Equal(updates, want) {
		t.Errorf("writeAPI updates = %v, want exactly %v", updates, want)
	}
}

// TestVersionReadersAreInspectOnly keeps each read used by an update or a
// removal to a single call, so none of them can grow into a write path.
func TestVersionReadersAreInspectOnly(t *testing.T) {
	readers := map[string]reflect.Type{
		"configReader":  reflect.TypeOf((*configReader)(nil)).Elem(),
		"serviceReader": reflect.TypeOf((*serviceReader)(nil)).Elem(),
		"secretReader":  reflect.TypeOf((*secretReader)(nil)).Elem(),
	}
	want := map[string][]string{
		"configReader":  {"ConfigInspect"},
		"serviceReader": {"ServiceInspect"},
		"secretReader":  {"SecretInspect"},
	}
	for name, typ := range readers {
		var names []string
		for i := range typ.NumMethod() {
			names = append(names, typ.Method(i).Name)
		}
		if !slices.Equal(names, want[name]) {
			t.Errorf("%s methods = %v, want exactly %v", name, names, want[name])
		}
	}
}
