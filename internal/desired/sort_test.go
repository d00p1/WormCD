package desired

import (
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func unencodableService(name string) Service {
	return Service{Name: name, Image: "img", Update: UpdateConfig{MaxFailureRatio: float32(math.NaN())}}
}

// images lists the image of each service. Unlike comparing whole services, it
// works when a service holds NaN, which reflect.DeepEqual never considers equal.
func images(services []Service) []string {
	out := make([]string, len(services))
	for i, s := range services {
		out[i] = s.Image
	}
	return out
}

func TestSortUnencodableTieReturnsErrorAndKeepsOrder(t *testing.T) {
	services := []Service{
		{Name: "web", Image: "web:2"},
		unencodableService("web"),
	}

	err := SortServices(services)
	if err == nil {
		t.Fatal("SortServices(NaN tie) returned no error")
	}
	if !strings.Contains(err.Error(), `Service "web"`) {
		t.Errorf("error does not name the element: %v", err)
	}
	if got, want := images(services), []string{"web:2", "img"}; !slices.Equal(got, want) {
		t.Errorf("SortServices reordered input on error: got %v, want %v", got, want)
	}
}

func TestSortUnencodableNonTieStillSorts(t *testing.T) {
	services := []Service{unencodableService("web"), {Name: "api"}}
	if err := SortServices(services); err != nil {
		t.Fatalf("SortServices(unique names): %v", err)
	}
	if services[0].Name != "api" {
		t.Errorf("services = %v, want api first", services[0].Name)
	}
}

func TestSortUnencodableLeavesEveryCollectionUnchanged(t *testing.T) {
	d := Desired{
		Services: []Service{{Name: "api", Image: "api:1"}, unencodableService("api")},
		Networks: []Network{{Name: "back", Driver: "overlay"}, {Name: "front", Driver: "overlay"}},
		Volumes:  []Volume{{Name: "data", Driver: "local"}, {Name: "cache", Driver: "local"}},
	}
	networks := slices.Clone(d.Networks)
	volumes := slices.Clone(d.Volumes)

	err := d.Sort()
	if err == nil {
		t.Fatal("Sort(NaN) returned no error")
	}
	if !strings.Contains(err.Error(), `Service "api"`) {
		t.Errorf("error does not name the element: %v", err)
	}
	if got, want := images(d.Services), []string{"api:1", "img"}; !slices.Equal(got, want) {
		t.Errorf("Sort reordered services on error: got %v, want %v", got, want)
	}
	if !reflect.DeepEqual(d.Networks, networks) {
		t.Errorf("Sort reordered networks on error: %+v", d.Networks)
	}
	if !reflect.DeepEqual(d.Volumes, volumes) {
		t.Errorf("Sort reordered volumes on error: %+v", d.Volumes)
	}
}

func TestSortOrdersStateWithoutError(t *testing.T) {
	d := shuffled()
	if err := d.Sort(); err != nil {
		t.Fatalf("Sort: %v", err)
	}
	if d.Services[0].Name != "api" || d.Services[1].Name != "web" {
		t.Errorf("services not ordered by name: %s, %s", d.Services[0].Name, d.Services[1].Name)
	}
	if d.Networks[0].Name != "back" || d.Volumes[0].Name != "cache" {
		t.Errorf("networks or volumes not ordered: %s, %s", d.Networks[0].Name, d.Volumes[0].Name)
	}
}
