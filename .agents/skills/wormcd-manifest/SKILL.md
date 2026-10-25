---
name: wormcd-manifest
description: "Author WormCD deploy manifests: wormcd.yaml, compose.yaml and per-environment overlays for a Docker Swarm application. Use when asked to write, convert, review or fix a WormCD manifest, add or change a target/environment, or decide which Compose fields v0 supports."
---

One application lives under `<deploy-repo>/apps/<name>/` with a `wormcd.yaml`,
a base `compose.yaml`, optional `envs/<target>.compose.yaml` overlays, and any
config files. WormCD renders them into `Desired`; it never builds images and
never reads a registry. Read the working example in `examples/deploy-repo/`
and the manifest model in `docs/design/0001-v0-architecture.md` §4 before
writing.

## Inputs

Ask for anything missing; do not invent application facts:

- application name and target names (`staging`, `prod`, …), one cluster per target;
- image reference per environment (prod should use a digest);
- ports, environment variables, config file contents, external secret names;
- replicas, CPU/memory limits, placement constraints, healthcheck.

Secret values never go into git. If the user supplies one, stop and use an
external Docker secret instead.

## Files

- `wormcd.yaml` — the only WormCD-specific format: targets, clusters, overlays, `syncPolicy`.
- `compose.yaml` — environment-neutral base service definitions.
- `envs/<target>.compose.yaml` — a normal Compose override, merged with `docker compose -f base -f overlay` semantics.
- `configs/…` — payloads referenced by top-level `configs`.

## wormcd.yaml

```yaml
apiVersion: wormcd.dev/v1alpha1
kind: Application
metadata:
  name: web
spec:
  compose: compose.yaml
  targets:
    - name: prod
      cluster: prod-1
      overlay: envs/prod.compose.yaml
      syncPolicy:
        automated: true
        prune: true
        selfHeal: true
```

Rules the loader enforces:

- `apiVersion` and `kind` must match exactly; unknown fields are rejected.
- `metadata.name` and every `targets[].name` are lowercase alphanumeric with dashes.
- `spec.compose` and `targets[].overlay` are relative paths inside the app directory.
- At least one target; target names are unique; `cluster` is required (informational until M2).
- `syncPolicy` is parsed now and enforced in M3; include it as intent.

## Compose support

Every service needs `image` pinned to a tag or digest. Supported service
fields:

- `command`, `entrypoint`, `environment` (explicit values only), `labels`.
- `hostname`, `user`, `working_dir`, `stop_signal`, `stop_grace_period`, `read_only`.
- `ports`: `target`, `published` (single port), `protocol` (`tcp`/`udp`/`sctp`), `mode` (`ingress`/`host`), `name`.
- `networks` (declared, optional `aliases`) and named-volume or bind `volumes`.
- `configs` / `secrets` references with `source`, `target`, `uid`, `gid`, `mode`.
- `healthcheck`: `test`, `interval`, `timeout`, `retries`, `start_period`.
- `deploy`: `replicas` or `scale`, `mode` (`replicated`/`global`), `endpoint_mode` (`vip`/`dnsrr`), `labels`, `resources` (`cpus`/`memory`/`pids`), `restart_policy`, `placement`, `update_config`, `rollback_config`.

Top-level support:

- `networks`: `driver`, `driver_opts`, `attachable`, `internal`, `labels`, `name`. External networks accept only `name`; no IPAM.
- `volumes`: `driver`, `driver_opts`, `labels`, `name`. External volumes accept only `name`.
- `configs`: exactly one of `file` or `content`, plus optional `name`, `labels`. External configs accept only `name`.
- `secrets`: external only (`external: true` + `name`).

Common fields that v0 rejects: `build`, `depends_on`, `env_file`, `profiles`,
`logging`, `privileged`, `cap_add`/`cap_drop`, `devices`, service-level
`restart`, `pull_policy`, `platform`, `container_name`, `tmpfs` mounts,
`healthcheck.start_interval`, port `host_ip` and port ranges, and Compose
`!reset`/`!override` tags. The renderer names every rejected field;
`internal/manifest/unsupported.go` is the exhaustive list. Delete the field
rather than working around it.

## Overlays

- Merge only. `!reset` and `!override` are rejected.
- Change only what differs per environment: image digest, replicas, environment, resources, secret name.
- Top-level maps merge by key, so `secrets.tls_key.name` can differ per target.
- Keep the base free of environment names and production digests.

## Naming and labels

- Namespace is `<app>_<target>`; services become `<namespace>_<compose-key>`.
- Managed networks and volumes are `<namespace>_<key>`.
- Configs are content-addressed: `<name>_<sha256(data)[:12]>`; edit the file, not the config name.
- External objects keep their declared `name`.
- WormCD injects `com.wormcd.application` and `com.wormcd.target`; never set those by hand.

## Validate

Render every target; the renderer is the authority on support:

```sh
wormcd render --app web/prod --root <deploy-repo> --output json
wormcd render --app web/staging --root <deploy-repo>
```

With a Swarm manager available, check the plan (read-only):

```sh
wormcd diff --app web/prod --root <deploy-repo>
```

Iterate until every target renders and the plan contains only expected
changes. Do not hand-write the desired JSON.

## Done when

- Every target renders without errors.
- No rejected Compose field remains, every image is pinned, every secret is external.
- Base compose is environment-neutral; each overlay holds only its differences.
- You reported the created files, target names, and any assumption you could not confirm.

## Prompt template

```text
Собери WormCD-манифест для приложения <name>.
Deploy-репо: <path>; targets: <staging/prod/...>; кластеры: <...>.
Образы: <repo:tag> (prod — digest).
Порты: <...>. Env: <...>. Configs: <файлы>. Secrets: <external-имена>.
Масштаб и ресурсы: <replicas/cpus/memory>. Placement/healthcheck: <...>.
Соблюдай v0-подмножество Compose и merge-only overlays; секреты только external.
Проверь `wormcd render` для каждого target и покажи список файлов и допущений.
```
