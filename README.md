# WormCD

Declarative continuous delivery for Docker Swarm clusters.

WormCD watches a Git repository, renders Compose files with per-environment
overlays, and reconciles Docker Swarm clusters to the declared state — including
drift correction. It does not build images: CI builds them and commits the
image digest back to the deploy repository.

- Design: [docs/design/0001-v0-architecture.md](docs/design/0001-v0-architecture.md)
- Decisions: [docs/adr/](docs/adr/)
- Glossary: [GLOSSARY.md](GLOSSARY.md)

## Status

Early development. Compose/overlay rendering (`wormcd render`) and read-only
Swarm inspection are implemented; planning (`wormcd diff`) and reconciliation
are next.

## Development

Requires Go 1.27+.

```sh
make build
./bin/wormcd version
./bin/wormcd server --listen :8080   # GET /healthz
```

Render an application from a deploy repository (read-only, no cluster needed):

```sh
./bin/wormcd render --app web/prod --root /path/to/deploy-repo
./bin/wormcd render --file deploy/apps/web/wormcd.yaml --target prod --output json
```

Unit tests are hermetic. Docker integration tests are opt-in:

```sh
WORMCD_INTEGRATION=1 go test ./internal/cluster -run TestIntegrationPing -v
WORMCD_INTEGRATION=1 go test ./internal/runtime/swarm -run TestIntegrationInspect -v
```

The second test skips unless the daemon is a Swarm manager, and it never
mutates the cluster.
