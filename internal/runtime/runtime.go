// Package runtime defines the contract between the reconcile core and a
// container runtime (Swarm first, vanilla Docker later). See ADR-0003.
package runtime

import (
	"context"

	"github.com/d00p1/wormcd/internal/desired"
)

// Kind identifies a runtime implementation.
type Kind string

const (
	KindSwarm  Kind = "swarm"
	KindDocker Kind = "docker"
)

// Scope identifies the application target a runtime call applies to.
type Scope struct {
	Application string
	Target      string
}

// Observed is the normalized state read from a runtime. It contains only
// objects owned by the target (discovered by the WormCD labels). External
// networks, volumes, configs and secrets normally carry no ownership labels
// and are therefore not included; a labelled external object would be
// indistinguishable from a managed one.
type Observed struct {
	Application string
	Target      string
	Namespace   string
	Services    []desired.Service
	Networks    []desired.Network
	Configs     []desired.Config
	Secrets     []desired.Secret
	Volumes     []desired.Volume
}

// Sort puts every collection in a deterministic order.
func (o *Observed) Sort() {
	desired.SortServices(o.Services)
	desired.SortNetworks(o.Networks)
	desired.SortConfigs(o.Configs)
	desired.SortSecrets(o.Secrets)
	desired.SortVolumes(o.Volumes)
}

// Set is a set of object keys. Prune uses it to decide which managed objects
// must survive.
type Set map[desired.Key]struct{}

// Has reports whether the object of the given kind and name is in the set.
func (s Set) Has(kind desired.Kind, name string) bool {
	_, ok := s[desired.Key{Kind: kind, Name: name}]
	return ok
}

// Result reports what Apply changed in the cluster.
type Result struct {
	Created []string `json:"created,omitempty"`
	Updated []string `json:"updated,omitempty"`
	Deleted []string `json:"deleted,omitempty"`
}

// PruneResult reports what Prune removed and which unwanted objects it left in
// place. Skipped objects are not failures; the caller decides how to report them.
type PruneResult struct {
	Removed []string `json:"removed,omitempty"`
	Skipped []Skip   `json:"skipped,omitempty"`
}

// Skip is an unwanted object that Prune left in place, with the reason.
type Skip struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// Health summarizes whether a target has converged. Problems lists the
// reasons it has not.
type Health struct {
	Healthy  bool     `json:"healthy"`
	Problems []string `json:"problems,omitempty"`
}

// Runtime is the contract between the reconcile core and a container runtime.
type Runtime interface {
	Kind() Kind
	Inspect(ctx context.Context, scope Scope) (Observed, error)
	Apply(ctx context.Context, plan Plan) (Result, error)
	Prune(ctx context.Context, scope Scope, keep Set) (PruneResult, error)
	Health(ctx context.Context, scope Scope) (Health, error)
}
