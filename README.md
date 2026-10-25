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

Early development. Compose/overlay rendering (`wormcd render`), read-only
Swarm inspection and read-only planning (`wormcd diff`) are implemented;
reconciliation (apply) is next.

| Capability | Status |
|---|---|
| Write `wormcd.yaml` + Compose + overlay, render `Desired` | `wormcd render` |
| Inspect a Swarm cluster read-only | `Runtime.Inspect` (internal) |
| Plan `Desired` vs `Observed` (`Create`/`Update`/`Delete`/`Noop`) | `wormcd diff`, exit `2` on drift |
| Apply, prune, self-heal | not yet (M1) |
| Fetch Git sources, multi-cluster targets | not yet (M2) |
| Webhooks, `syncPolicy`, rollback | not yet (M3) |
| Registry polling for new image versions | not planned: CI builds the image and commits the digest to the deploy repository |

To try it now, write the manifests in a local checkout, create the labelled
objects described in [cmd/wormcd/testdata/stand/README.md](cmd/wormcd/testdata/stand/README.md),
and run `wormcd diff`. A complete, renderable deploy repo lives in
[examples/deploy-repo](examples/deploy-repo/). WormCD does not deploy yet — use
`docker stack deploy` for the workload.

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

Plan a target against a Swarm cluster (read-only):

```sh
./bin/wormcd diff --app web/prod --root /path/to/deploy-repo
./bin/wormcd diff --app web/prod --root /path/to/deploy-repo --output json
```

`diff` is read-only and, until the cluster registry arrives in M2, connects to
the local Docker socket (`unix:///var/run/docker.sock`). Exit codes: `0` — no
differences, `2` — the plan has changes, `1` — error.

Unit tests are hermetic. Docker integration tests are opt-in:

```sh
WORMCD_INTEGRATION=1 go test ./internal/cluster -run TestIntegrationPing -v
WORMCD_INTEGRATION=1 go test ./internal/runtime/swarm -run TestIntegrationInspect -v
WORMCD_INTEGRATION=1 go test ./cmd/wormcd -run TestIntegrationDiff -v
```

`TestIntegrationInspect` is read-only. `TestIntegrationInspectStack` and
`TestIntegrationDiff` create a disposable labelled stack, run the operation and
remove it again; both skip unless the daemon is a Swarm manager. The diff stand
is also available for manual runs — see
[cmd/wormcd/testdata/stand/README.md](cmd/wormcd/testdata/stand/README.md).

For a remote daemon or a dind Swarm, point the tests at it with mTLS:

```sh
WORMCD_INTEGRATION=1 \
WORMCD_DOCKER_ENDPOINT=tcp://host:2376 \
WORMCD_DOCKER_CA=ca.pem WORMCD_DOCKER_CERT=cert.pem WORMCD_DOCKER_KEY=key.pem \
go test ./...
```
