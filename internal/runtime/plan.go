package runtime

import "github.com/d00p1/wormcd/internal/desired"

// Action is what apply must do with one object.
type Action string

const (
	Create Action = "Create"
	Update Action = "Update"
	Delete Action = "Delete"
	Noop   Action = "Noop"
)

// Entry is one object in a plan. Key is "Kind/name", the same key the diff
// engine uses to match desired and observed objects.
type Entry struct {
	Key     string   `json:"key"`
	Action  Action   `json:"action"`
	Changes []string `json:"changes,omitempty"`
}

// Summary counts the actions in a plan.
type Summary struct {
	Create int `json:"create"`
	Update int `json:"update"`
	Delete int `json:"delete"`
	Noop   int `json:"noop"`
}

// Plan is the ordered difference between desired and observed state. It is
// the input for Runtime.Apply in M1. Entries are grouped by kind in apply
// order and sorted by name inside a kind, so the same inputs always produce
// the same plan.
type Plan struct {
	Application string  `json:"application"`
	Target      string  `json:"target"`
	Namespace   string  `json:"namespace"`
	Summary     Summary `json:"summary"`
	Entries     []Entry `json:"entries"`

	// Desired carries the object contents Apply needs (config data, labels,
	// driver options). It is not part of the plan's JSON output.
	Desired desired.Desired `json:"-"`
}

// HasChanges reports whether applying the plan would mutate the cluster.
func (p Plan) HasChanges() bool {
	return p.Summary.Create > 0 || p.Summary.Update > 0 || p.Summary.Delete > 0
}

// Add appends an entry and updates the summary.
func (p *Plan) Add(entry Entry) {
	p.Entries = append(p.Entries, entry)
	switch entry.Action {
	case Create:
		p.Summary.Create++
	case Update:
		p.Summary.Update++
	case Delete:
		p.Summary.Delete++
	case Noop:
		p.Summary.Noop++
	}
}
