# Example deploy repository

A complete, renderable WormCD application. It follows the deploy-repo layout
from ADR-0005, so it doubles as the reference for the `wormcd-manifest` skill.

```
deploy-repo/
└── apps/
    └── web/
        ├── wormcd.yaml              # targets, clusters, overlays, syncPolicy
        ├── compose.yaml             # environment-neutral base
        ├── configs/config.yaml      # content-addressed config payload
        └── envs/
            ├── staging.compose.yaml # smaller copy
            └── prod.compose.yaml    # pinned digest, more replicas, prod secret
```

## Render it

From the WormCD repository root:

```sh
go run ./cmd/wormcd render --app web/prod --root examples/deploy-repo
go run ./cmd/wormcd render --app web/staging --root examples/deploy-repo --output json
```

Both targets must render without a cluster. The output shows the normalized
`Desired` state: `wormcd_web_prod_web`, the `_default` overlay network, the
`data` volume, the content-addressed config (`..._<sha256[:12]>`) and the
external secret name.

## What it demonstrates

- **Base + overlay merge.** The base has no environment-specific values; each
  overlay changes only what differs. Merge is `docker compose -f base -f
  overlay`, no `!reset`/`!override`.
- **Pinned images.** The base uses a tag, prod uses a digest. CI rewrites that
  digest; WormCD never polls a registry.
- **Content-addressed configs.** `configs/config.yaml` becomes
  `<namespace>_app_config_<hash>`. Editing the file creates a new object.
- **External secrets only.** `tls_key` points at `staging_tls_key` in the base
  and `prod_tls_key` in the prod overlay. Secret values never enter git.
- **Swarm deploy fields.** Healthcheck, resources, restart policy, update
  config and placement constraints all map onto the normalized model.

## Run it against a local Swarm

M1 can apply this repo. From the WormCD repository root, with a Swarm
manager available:

```sh
make build
docker swarm init   # once, if the node is not a manager yet

# v0 supports external Docker secrets only: create them before syncing.
printf 'demo-tls-key' | docker secret create prod_tls_key -
printf 'demo-tls-key' | docker secret create staging_tls_key -   # for web/staging

./bin/wormcd diff --app web/prod --root examples/deploy-repo   # preview, exit 2 on drift
./bin/wormcd sync web/prod --root examples/deploy-repo --data /tmp/wormcd.db
./bin/wormcd status web/prod --root examples/deploy-repo --data /tmp/wormcd.db
./bin/wormcd history web/prod --data /tmp/wormcd.db
```

Two things can stop a `sync` on a toy single-node Swarm:

- **Placement.** The base compose demonstrates placement constraints
  (`node.role == worker`), so the service schedules only when a worker node
  joins. On a manager-only node, copy this directory to a scratch path,
  remove that constraint, and point `--root` at the copy.
- **Image.** `ghcr.io/d00p1/wormcd-demo` is a private image (prod pins a
  digest, staging a tag). When the daemon cannot pull it, the service never
  converges and `sync` waits until its run deadline (30 minutes by default),
  so cancel it early and check
  `docker service ps wormcd_web_prod_web --no-trunc` for the task's error
  (for example `No such image`).

The read-only variant of this stand (hand-labelled objects, `replicas: 0`, no
image pull) lives in
[`cmd/wormcd/testdata/stand/`](../../cmd/wormcd/testdata/stand/README.md).
