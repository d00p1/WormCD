# WormCD v0 — архитектура

**Статус:** draft
**Дата:** 2026-10-25

Зафиксированные решения:

- **Pull-based GitOps.** Контроллер читает git, приводит кластеры к описанному состоянию.
- **CI коммитит digest образа в git.** Образы собирает CI, WormCD их не собирает и за registry не следит.
- **Compose + overlay** — источник истины для приложения.
- **Swarm-first**, но всё применение идёт через интерфейс `Runtime`; vanilla Docker — следующий рантайм.
- **Один control plane на N Swarm-кластеров**, у каждого кластера свои credentials и env-overlay.
- **Манифесты — в отдельном deploy-репозитории**, не в репозитории приложения.
- **Удалённые кластеры — Docker API по mTLS**; управляющий кластер — через локальный unix socket.
- **Control plane живёт сервисом в одном из Swarm-кластеров** (управляющий кластер).

## 1. Что делает WormCD

WormCD — control plane декларативного CD для Docker Swarm (позже — vanilla Docker).

Он не собирает образы и не является CI. Его работа:

1. прочитать git и получить желаемое состояние;
2. сравнить с фактическим состоянием кластера;
3. применить разницу, дождаться сходимости, записать ревизию;
4. откатить дрейф, если состояние кластера ушло от git.

Поток целиком:

```
commit ──► CI ──► registry (image@sha256:...)
             └──► commit digest в git (overlay)
                        │
                        ▼
                WormCD control plane
                  ├─ clone/fetch source
                  ├─ render compose + overlay
                  ├─ diff desired vs observed
                  └─ apply / prune / health / history
                        │
        ┌───────────────┼───────────────┐
        ▼               ▼               ▼
   swarm-prod-1    swarm-staging    swarm-edge
```

## 2. Non-goals v0

- сборка и push образов (это CI);
- Kubernetes;
- canary/blue-green (ограничения Swarm; вернёмся отдельным дизайном);
- web UI, RBAC, OIDC, HA control plane;
- управление секретами приложений сверх Docker secrets;
- произвольные плагины и шаблонизаторы (Helm-подобный слой).

## 3. Компоненты

Один бинарник `wormcd` с подкомандами:

- `wormcd server` — control plane: API, git-кэш, планировщик, reconciler, SQLite.
- `wormcd ...` — CLI, тонкий клиент к API (`cluster`, `source`, `apps`, `sync`, `status`, `history`, `rollback`).
- `wormcd agent` — появится позже, для кластеров без доступа к Docker API.

Пакеты:

```
cmd/wormcd
internal/api          REST, auth, webhooks
internal/cluster      Docker API-клиенты, TLS, credentials
internal/git          clone/fetch, auth, разрешение ревизии
internal/manifest     compose-go, merge overlay, парсинг wormcd.yaml
internal/desired      нормализованная модель желаемого состояния
internal/runtime      интерфейс Runtime
internal/runtime/swarm
internal/runtime/docker   позже
internal/reconcile    diff, apply, prune, health, rollback
internal/store        SQLite
```

Зависимости-кандидаты: `github.com/compose-spec/compose-go/v2`, `github.com/docker/docker/client`, `modernc.org/sqlite` (без cgo), `github.com/go-chi/chi/v5`.

## 4. Модель манифестов

Git — единственный источник истины. Control plane хранит только регистрацию source-репозиториев и credentials кластеров.

Манифесты живут в **отдельном deploy-репозитории** (ADR-0005), а не в репозитории приложения. Это разделяет права: CI приложения пушит образ и коммитит digest в deploy-репо, но не имеет права писать в код, а деплой-история не смешивается с историей разработки.

Раскладка deploy-репозитория:

```
deploy/
└── apps/
    └── web/
        ├── compose.yaml
        ├── wormcd.yaml
        └── envs/
            ├── staging.compose.yaml
            └── prod.compose.yaml
```

Source регистрируется как «репозиторий + ветка + путь, где искать `wormcd.yaml`». Добавление приложения = новый файл в git, удаление = удаление файла (с pruning).

`wormcd.yaml` описывает приложение и его target-ы:

```yaml
apiVersion: wormcd.dev/v1alpha1
kind: Application
metadata:
  name: web
spec:
  compose: compose.yaml
  targets:
    - name: staging
      cluster: staging-1
      overlay: envs/staging.compose.yaml
      syncPolicy:
        automated: true
        prune: true
        selfHeal: true
    - name: prod
      cluster: prod-1
      overlay: envs/prod.compose.yaml
      syncPolicy:
        automated: true
        prune: true
        selfHeal: true
```

Overlay — **обычный compose override**, а не собственный язык. Семантика слияния — как у `docker compose -f compose.yaml -f overlay.yaml`. Это даёт совместимость с существующими compose-файлами и возможность локально проверить результат через `docker compose config`.

```yaml
# envs/prod.compose.yaml
services:
  web:
    image: ghcr.io/d00p1/wormcd-demo@sha256:9f2c...
    deploy:
      replicas: 3
      resources:
        limits:
          cpus: "0.5"
          memory: 256M
    environment:
      APP_ENV: prod
```

Digest в overlay обновляет CI. Пример шага CI (не часть WormCD):

```bash
yq -i '.services.web.image = "ghcr.io/d00p1/wormcd-demo@'"$DIGEST"'"' \
  deploy/apps/web/envs/prod.compose.yaml
git commit -am "deploy(web/prod): $DIGEST" && git push
```

Ограничения v0:

- поддерживаются только возможности Compose, которые WormCD умеет применить к Swarm; `build`, `depends_on` как runtime-зависимость и т.п. отклоняются с внятной ошибкой;
- используется только merge-семантика, без `!reset`/`!override`; теги отклоняются при загрузке;
- образы обязаны быть закреплены тегом или digest — иначе деплой недетерминирован;
- секреты приложения — только external Docker secrets, значения в git не попадают;
- configs адресуются по содержимому (`<name>_<sha256[:12]>`), поэтому изменение файла создаёт новый объект;
- named volumes входят в нормализованную модель наряду с networks/configs/secrets.

## 5. Runtime interface

Ядро не знает про Swarm. Оно работает с нормализованной моделью и интерфейсом:

```go
type Scope struct {
    Application string
    Target      string
}

type Runtime interface {
    Kind() Kind
    Inspect(ctx context.Context, scope Scope) (Observed, error)
    Apply(ctx context.Context, plan Plan) (Result, error)
    Prune(ctx context.Context, app string, keep Set) error
    Health(ctx context.Context, app string) (Health, error)
}

type Desired struct {
    Services []Service
    Networks []Network
    Configs  []Config
    Secrets  []Secret
    Volumes  []Volume
}
```

В M0 реализуется только `Inspect`; `Apply`, `Prune` и `Health` появляются в M1.

Ключи объектов: `kind/name` внутри namespace приложения. Все создаваемые объекты получают метку `com.wormcd.application=<app>` и `com.wormcd.target=<target>`; по ним же фильтруется observed state и работает prune.

Swarm-реализация:

- `Inspect` — services/configs/secrets/networks/volumes, отфильтрованные по меткам;
- `Apply` — создание сетей, создание configs/secrets, `ServiceUpdate` для сервисов;
- configs/secrets иммутабельны: новое содержимое = новое имя с хешем, сервис переключается, старые удаляются после успешной сходимости;
- `Health` — desired replicas vs running, статус update, ошибки задач; опционально HTTP-проба с control plane (у Swarm нет readiness).

Порядок применения: `networks → configs/secrets → services (с учётом dependsOn) → prune`. Каждый шаг идемпотентен. Ошибка останавливает sync, target уходит в `Degraded`, предыдущая ревизия остаётся доступной для rollback.

## 6. Состояние и история

Желаемое состояние каждый раз рендерится из git; в БД кэшируется только наблюдаемое и история.

```
clusters(id, name, endpoint, ca_ref, cert_ref, key_ref, labels)
sources(id, repo_url, branch, path, auth_ref)
targets(id, app, target, cluster, last_revision, status, message)
revisions(id, target_id, number, commit, rendered_spec, image_digests, created_at)  -- UNIQUE(target_id, number)
events(id, target_id, revision_id, type, payload, created_at)
```

`revisions.number` — порядковый номер ревизии внутри target (1, 2, …). Пара `(target_id, number)` уникальна. Номер, а не внутренний `id`, — то, чем оперирует пользователь: `rollback --revision N` и `history` ссылаются именно на него. UNIQUE не даёт двум параллельным sync выдать один номер.

`revisions.rendered_spec` хранит полностью отрендеренный `Desired` в JSON — это основа diff-а и rollback без похода в git. Байты записи получаются через `desired.Canonical`: функция сортирует коллекции в копии `Desired` (не мутируя вход) и маршалит её, ключи map идут в отсортированном порядке, поэтому одинаковый рендер даёт побайтово одинаковую запись. Store только проверяет, что это валидный JSON, и не зависит от `desired` (правило `.go-arch-lint.yml`): вызывающий код передаёт ему уже канонические байты.

Статусы target: `Unknown`, `Synced`, `OutOfSync`, `Progressing`, `Healthy`, `Degraded`, `Blocked` (нет кластера/невалидный манифест).

## 7. Multi-cluster и доступ

Cluster — запись с endpoint и credentials. Control plane обращается к Docker Engine API каждого кластера напрямую. Транспорт зафиксирован в ADR-0004: удалённые кластеры — mTLS на 2376, управляющий кластер — локальный `unix:///var/run/docker.sock`.

| Вариант доступа | Плюсы | Минусы |
|---|---|---|
| Docker API по mTLS (2376) | нативно, минимум компонентов | нужно открыть мощный endpoint |
| SSH-туннель | API не публикуется | хрупко, привязка к SSH |
| Агент в кластере | безопасно, работает без входящего доступа | ещё один компонент |

Для v0 выбран mTLS + отдельный CA на кластер + сетевой ACL/VPN. Агент — отдельный milestone для кластеров, куда control plane не достаёт.

Control plane (ADR-0006) запускается сервисом с одной репликой в управляющем кластере:

- SQLite и git-кэш — на persistent volume, привязанном к узлу; HA не входит в v0;
- локальный кластер подключается через смонтированный socket, удалённые — по mTLS;
- обновление самого WormCD до M5 выполняется вручную (`docker service update`), а не через WormCD: self-sync control plane создаёт цикл «перезапуск посреди sync»;
- падение управляющего кластера останавливает управление остальными — это принятая цена за отсутствие отдельной VM в v0.

Важно: Docker API — это root-эквивалент. Ограничение прав делается authorization-плагином Swarm и сетевыми политиками, а не логикой WormCD.

## 8. API и CLI

```
GET    /api/v1/apps
GET    /api/v1/apps/{app}
GET    /api/v1/apps/{app}/targets/{target}
POST   /api/v1/apps/{app}/targets/{target}/sync
POST   /api/v1/apps/{app}/targets/{target}/rollback
GET    /api/v1/apps/{app}/targets/{target}/history
POST   /api/v1/hooks/git/{source}          # webhook от GitHub/GitLab
GET    /api/v1/clusters
POST   /api/v1/clusters
```

CLI:

```
wormcd cluster add prod-1 --endpoint tcp://10.0.0.5:2376 --ca ... --cert ... --key ...
wormcd source add deploy --repo git@github.com:d00p1/deploy.git --branch main --path apps
wormcd apps
wormcd status web/prod
wormcd sync web/prod
wormcd history web/prod
wormcd rollback web/prod --revision 42
```

Аутентификация API в v0 — bearer token. OIDC и RBAC — позже.

## 9. Этапы

| Этап | Результат | Критерий готовности |
|---|---|---|
| M0 | скелет, парсинг compose+overlay, нормализация, dry-run diff | `wormcd diff` показывает план против реального Swarm, ничего не меняя |
| M1 | apply/prune, drift, история ревизий, один кластер | сервис создаётся/обновляется/удаляется, ручное изменение откатывается |
| M2 | sources, обнаружение `wormcd.yaml`, overlay-окружения, N кластеров | один и тот же app катится в staging и prod в разные кластеры |
| M3 | syncPolicy (automated/prune/selfHeal), webhooks, health gates, rollback | push в git запускает sync; rollback возвращает предыдущий rendered spec |
| M4 | vanilla Docker runtime | тот же app применяется к одиночному Docker через `Runtime` |
| M5 | web UI, agent mode, HA, OIDC/RBAC | — |

M0 специально read-only: сначала доказываем, что diff корректен, и только потом даём право что-то менять.

## 10. Открытые вопросы

1. **Секреты приложений.** Только Docker secrets в v0 или сразу внешний провайдер (Vault/SOPS)?
2. **UI.** Достаточно ли CLI+API до M5?
3. **Репозиторий для deploy.** Нужен ли отдельный GitHub-репозиторий уже для M0, или на старте достаточно локального git-репозитория?
4. **Бэкап SQLite.** Достаточно ли периодического снапшота на volume или нужен внешний бэкап с первого дня?

## 11. Что дальше

- Создать deploy-репозиторий и определить его раскладку.
- Разложить M0 на issues в GitHub.
- Начать с `Runtime.Inspect` + нормализации compose: без этого diff не на что опереть.
