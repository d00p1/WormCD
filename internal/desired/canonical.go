package desired

import (
	"encoding/json"
	"slices"
)

// Canonical returns the canonical JSON encoding of d. Collections are put in
// deterministic order and encoding/json sorts map keys, so equal desired states
// produce identical bytes. The store keeps these bytes as a revision's rendered
// spec. Canonical does not modify d. It fails if d cannot be encoded, such as a
// NaN float, before sorting, because the sort compares elements by encoding.
func Canonical(d *Desired) ([]byte, error) {
	if _, err := json.Marshal(d); err != nil {
		return nil, err
	}
	c := d.clone()
	c.Sort()
	return json.Marshal(c)
}

// clone copies every slice that Sort reorders, including the nested slices of
// each service, so that sorting the copy leaves d unchanged.
func (d *Desired) clone() *Desired {
	c := *d
	c.Services = slices.Clone(d.Services)
	for i := range c.Services {
		s := &c.Services[i]
		s.Ports = slices.Clone(s.Ports)
		s.Networks = slices.Clone(s.Networks)
		s.Mounts = slices.Clone(s.Mounts)
		s.Configs = slices.Clone(s.Configs)
		s.Secrets = slices.Clone(s.Secrets)
	}
	c.Networks = slices.Clone(d.Networks)
	c.Configs = slices.Clone(d.Configs)
	c.Secrets = slices.Clone(d.Secrets)
	c.Volumes = slices.Clone(d.Volumes)
	return &c
}
