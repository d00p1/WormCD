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

## Trying a diff

M0 cannot deploy yet (`apply` is M1), and `wormcd diff` only sees objects
labelled `com.wormcd.application` / `com.wormcd.target`. To exercise the plan
against a real cluster, use the labelled stand in
[`cmd/wormcd/testdata/stand/`](../../cmd/wormcd/testdata/stand/README.md)
or create the objects manually with those labels. `docker stack deploy` alone
is not enough: it neither names objects with the `<app>_<target>_` prefix nor
attaches the WormCD labels.
