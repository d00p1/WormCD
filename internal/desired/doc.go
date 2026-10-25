// Package desired defines the normalized desired-state model rendered from
// Compose files and environment overlays.
//
// The model is runtime-agnostic: the Swarm and vanilla Docker runtimes map it
// onto their own primitives. See ADR-0002 and issue #2.
package desired
