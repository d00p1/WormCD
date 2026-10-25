package desired

// Managed reports whether an object belongs to the target and is therefore
// planned by WormCD. External clusters, networks, volumes, configs and secrets
// are referenced by a target but are not owned by it.
type Managed interface {
	Managed() bool
}

// Managed reports whether the network is owned by the target.
func (n Network) Managed() bool { return !n.External }

// Managed reports whether the config is owned by the target.
func (c Config) Managed() bool { return !c.External }

// Managed reports whether the secret is owned by the target.
func (s Secret) Managed() bool { return !s.External }

// Managed reports whether the volume is owned by the target.
func (v Volume) Managed() bool { return !v.External }

// Managed reports whether the service is owned by the target. Services are
// always managed: v0 has no external service concept.
func (s Service) Managed() bool { return true }
