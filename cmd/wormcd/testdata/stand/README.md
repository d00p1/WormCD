# Diff test stand

A minimal application for exercising `wormcd diff` against a real, local
Swarm. `replicas: 0` keeps the service from pulling the image or starting
tasks.

The automated version of this stand is `TestIntegrationDiff` in
`cmd/wormcd/diff_integration_test.go`:

```sh
docker swarm init   # once, if the node is not a manager yet
WORMCD_INTEGRATION=1 go test ./cmd/wormcd -run TestIntegrationDiff -v
```

For a manual run, create the objects the plan expects and label them so that
`Inspect` can find them:

```sh
docker network create --driver overlay \
  --label com.wormcd.application=web \
  --label com.wormcd.target=prod \
  wormcd_web_prod_default

docker service create --detach --replicas 0 \
  --name wormcd_web_prod_web \
  --label com.wormcd.application=web \
  --label com.wormcd.target=prod \
  --network wormcd_web_prod_default \
  ghcr.io/d00p1/wormcd-demo:v1
```

The target is in sync:

```sh
./bin/wormcd diff --app web/prod --root cmd/wormcd/testdata/stand
# exit code 0
```

Drift and a service that is absent from the Compose file show up as `Update`
and `Delete`:

```sh
docker service update --image ghcr.io/d00p1/wormcd-demo:manual wormcd_web_prod_web
docker service create --detach --replicas 0 \
  --name wormcd_web_prod_old \
  --label com.wormcd.application=web \
  --label com.wormcd.target=prod \
  --network wormcd_web_prod_default \
  ghcr.io/d00p1/wormcd-demo:v1

./bin/wormcd diff --app web/prod --root cmd/wormcd/testdata/stand
# Update  Service/wormcd_web_prod_web (image)
# Delete  Service/wormcd_web_prod_old
# exit code 2
```

Clean up:

```sh
docker service rm wormcd_web_prod_old wormcd_web_prod_web
docker network rm wormcd_web_prod_default
```
