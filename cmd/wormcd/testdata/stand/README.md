# Diff test stand

A minimal application for exercising `wormcd diff` against a real, local
Swarm. It covers a service, its default network, a config, an external secret,
a named volume and a published port. `replicas: 0` keeps the service from
pulling the image or starting tasks.

The automated version of this stand is `TestIntegrationDiff` in
`cmd/wormcd/diff_integration_test.go`:

```sh
docker swarm init   # once, if the node is not a manager yet
WORMCD_INTEGRATION=1 go test ./cmd/wormcd -run TestIntegrationDiff -v
```

For a manual run, create the objects the plan expects. The config name carries
the content hash and must match the rendered name:

```sh
printf 'stand\n' > /tmp/stand-config

docker network create --driver overlay \
  --label com.wormcd.application=web \
  --label com.wormcd.target=prod \
  wormcd_web_prod_default

docker config create \
  --label com.wormcd.application=web \
  --label com.wormcd.target=prod \
  wormcd_web_prod_app_9161857194af /tmp/stand-config

printf 'stand-secret' | docker secret create prod_tls -

docker volume create \
  --label com.wormcd.application=web \
  --label com.wormcd.target=prod \
  wormcd_web_prod_data

docker service create --detach --replicas 0 \
  --name wormcd_web_prod_web \
  --label com.wormcd.application=web \
  --label com.wormcd.target=prod \
  --network wormcd_web_prod_default \
  --publish 8080:80 \
  --config source=wormcd_web_prod_app_9161857194af,target=/etc/wormcd/config \
  --secret source=prod_tls,target=/run/secrets/tls \
  --mount type=volume,source=wormcd_web_prod_data,target=/data \
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
docker volume rm wormcd_web_prod_data
docker secret rm prod_tls
docker config rm wormcd_web_prod_app_9161857194af
docker network rm wormcd_web_prod_default
rm /tmp/stand-config
```
