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

Requires Go 1.27+ and [mise](https://mise.jdx.dev/). The toolchain and every
linter version are pinned in `mise.toml`:

```sh
mise install              # install the pinned toolchain
mise run install-hooks    # install git hooks, once per clone
mise run check            # every check; this is the same command CI runs
mise run fix              # apply safe autofixes, then review the diff
```

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

`TestIntegrationInspect` is read-only. `TestIntegrationInspectStack` creates a
disposable labelled stack and removes it again; both skip unless the daemon is
a Swarm manager.

For a remote daemon or a dind Swarm, point the tests at it with mTLS:

```sh
WORMCD_INTEGRATION=1 \
WORMCD_DOCKER_ENDPOINT=tcp://host:2376 \
WORMCD_DOCKER_CA=ca.pem WORMCD_DOCKER_CERT=cert.pem WORMCD_DOCKER_KEY=key.pem \
go test ./...
```
