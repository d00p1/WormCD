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

// Runtime is the contract between the reconcile core and a container runtime.
// M0 only needs to read state; Apply, Prune and Health arrive in M1.
type Runtime interface {
	Kind() Kind
	Inspect(ctx context.Context, scope Scope) (Observed, error)
}
