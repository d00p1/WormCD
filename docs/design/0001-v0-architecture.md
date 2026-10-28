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
    Prune(ctx context.Context, scope Scope, keep Set) (PruneResult, error)
    Health(ctx context.Context, scope Scope) (Health, error)
}

type PruneResult struct {
    Removed []string // ключи удалённых объектов
    Skipped []Skip   // нежелательные объекты, которые Prune оставил, с причиной
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
- `Apply` — создание сетей, configs/secrets и volumes; затем сервисы: удаление сервисов с действием Delete (`ServiceRemove`), `ServiceCreate` и `ServiceUpdate` с полным spec. Spec строится из desired чистой функцией, обратной к `Inspect`; ссылки на configs и secrets идут по Docker ID, которые Apply берёт через Inspect. Каждый удаляемый и обновляемый сервис проверяется на принадлежность скоупу по меткам перед вызовом API; preflight до первой мутации проверяет план сервисов (записи, ссылки, mode);
- сходимость: сервис сошёлся, когда задачи его текущей ревизии (spec равен `Service.Spec.TaskTemplate`, созданы не раньше `Service.UpdatedAt`) выполняют desired tasks, ни одна из них ещё не стартует и update не идёт. Старые задачи не считаются, даже если running: после `ServiceUpdate` они работают до того, как updater их остановит, и `UpdateStatus` сразу после вызова не надёжен. Упавшая задача текущей ревизии (включая crash loop, где Swarm уже пометил её shutdown) и paused или rollback update — ошибка Apply сразу, без ожидания дедлайна. Предположение: демон хранит spec без нормализации; проверяется интеграционным тестом (#22). Дедлайн и интервал опроса — параметры runtime;
- упавшая задача текущей ревизии (`failed` или `rejected`) — ошибка сходимости, в том числе при crash loop и даже когда её замена уже запущена (Swarm поднял замену, Apply и Health всё равно сообщают об ошибке). Это явное решение, не недосмотр: устранение — новая ревизия или ручная очистка задач;
- откат: Apply возвращает ошибку, если у обновления этой ревизии статус `paused`, `rollback_started`, `rollback_paused` или `rollback_completed`, даже когда задачи запущены; причина попадает в ошибку вместе с ошибками задач;
- удаление сервисов делает Apply, а не Prune: план уже решил Delete, а Prune идёт после Apply, когда старые configs и secrets уже никем не используются. Перед `ServiceRemove` сервис проверяется на метки скоупа. Уже исчезнувший сервис (NotFound на inspect или remove) — не ошибка, и в `Deleted` не попадает. Prune не удаляет сервисы; сервис скоупа вне `keep`, который после Apply ещё существует, попадает в `Skipped` с причиной «still present after Apply, and not in the desired state; Apply should have removed it». Такое бывает только при устаревшем плане: при ошибке Apply sync останавливается до Prune;
- configs: данные иммутабельны. Новое содержимое = новое имя с хешем (`<name>_<sha256[:12]>`), сервис переключается, старые удаляются после успешной сходимости. Labels config меняются на месте через `ConfigUpdate` (имя не меняется, Diff даёт Update с `labels.<key>`). `ConfigUpdate` требует текущую версию объекта, поэтому Apply берёт её из Inspect, и Spec с теми же data и обновлёнными labels; любое другое изменение Update отклоняется до вызова API;
- optimistic lock для `ConfigUpdate` не используем (вариант A): Version берётся из Inspect в момент Apply, а не из Plan. Если labels поменяли между Diff и Apply, Apply перезапишет правку целевыми labels — это соответствует desired state (selfHeal). Опасно только при параллельных sync одного target с разными desired. Условие: в M1 (#20) sync одного target сериализуется lock-ом; без этого решение неверно. Пересмотреть при появлении нескольких источников запуска sync (webhook и ручной sync в M3): перейти на Version из Plan с retry;
- Plan несёт Desired, чтобы Apply не получал второй источник правды; Apply проверяет совпадение Application/Target плана и Desired;
- Update сетей и volumes — ошибка в M1: in-place update в v0 не поддерживается. Для сети ошибка велит отключить её сервисы и пересоздать сеть вручную. Для volume ошибка предупреждает о потере данных: пересоздание удаляет их, поэтому сначала бэкап, затем пересоздание вручную; см. #23;
- secrets иммутабельны и в v0 не создаются (managed secrets отклоняет normalize), поэтому `SecretUpdate` не реализован: Diff не даёт для них Update, и изменение их labels не вызывает действий (это поведение, а не ошибка; в v0 secrets только external);
- `Prune` удаляет networks, configs и secrets, которые не в `keep` и не используются ни одним сервисом кластера (Docker всё равно не даст удалить используемый объект). Пропущенные объекты попадают в `PruneResult.Skipped` с причиной;
- volumes не удаляются автоматически: в них данные. Prune оставляет неиспользуемый volume и перечисляет его в `Skipped`; удаляет его человек;
- `Health` — для каждого сервиса скоупа: задачи текущей ревизии против desired tasks, статус update (paused, rollback), ожидающие старта задачи и ошибки задач текущей ревизии (текст ошибки задачи попадает в `Problems`, в том числе crash loop). `Healthy` — все сервисы сошлись без проблем. Скоуп без единого объекта — `Healthy: false` с причиной «no objects found»; скоуп с объектами, но без сервисов, здоров. Опционально HTTP-проба с control plane (у Swarm нет readiness) — не в M1;

Порядок применения: `networks → configs/secrets → volumes → удаление сервисов → services → prune`. Внутри шага сервисы не зависят друг от друга: `depends_on` отклоняется при рендере (§4), порядок сервисов внутри шага не важен для v0. Volumes создаются до сервисов, потому что сервисы их монтируют; prune идёт после сходимости. Каждый шаг идемпотентен. Ошибка останавливает sync, target уходит в `Degraded`, предыдущая ревизия остаётся доступной для rollback.

Sync и откат (M1, #20). `reconcile.Service` делает `Sync(app/target)`: render из локального checkout → `Inspect` → `Diff` → `Apply` (только если план меняет кластер) → `Prune` → `Health` → запись ревизии. `Apply` вызывается только если план меняет кластер; `Prune` идёт после `Apply` и не выполняется, если `Apply` упал. Health gate: если `Health` не `Healthy`, ревизия не записывается, target уходит в `Degraded` с проблемами в `message`, последняя `Synced`-ревизия остаётся последней. Кластер берётся из манифеста, Runtime создаёт фабрика, которую инжектирует composition root. Дедлайн общий для sync и rollback (`SyncTimeout`, по умолчанию 30 мин), считается после захвата lock и покрывает весь run; при истечении target уходит в `Degraded`, сообщение «sync deadline exceeded» или «rollback deadline exceeded». Commit в M1 пустой: git-источник появляется в M2. `Status(app/target)` делает только `Inspect` и `Diff` и возвращает `OutOfSync` или `Synced`, без записи в store.

`Rollback(app/target, N)` читает ревизию N, декодирует `rendered_spec` в `Desired` и проходит тот же путь `Diff → Apply → Prune → Health`. Результатом становится новая ревизия с номером больше последнего: история не переписывается. Событие `rollback.*` ссылается на N, и его сообщение говорит, что это откат к N.

Сериализация (ADR-0007): `Sync` и `Rollback` одного target выполняются под in-process lock по ключу `app/target`; разные target идут параллельно. Ожидание lock учитывает контекст. Status lock не берёт, поэтому во время sync он может показать промежуточное состояние.

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

`revisions.rendered_spec` хранит полностью отрендеренный `Desired` в JSON — это основа diff-а и rollback без похода в git. Store принимает отрендеренное состояние как значение, реализующее `Canonical() ([]byte, error)` (интерфейс `store.Canonicaler`); `desired.Desired` его реализует. `AppendRevision` сам вызывает `Canonical` и пишет полученные байты, поэтому каноничность гарантируется типом, а не соглашением вызывающего кода. Метод сортирует коллекции в копии `Desired` (не мутируя вход) и маршалит её, ключи map идут в отсортированном порядке, сортировка коллекций сравнивает полный ключ (при равенстве полей — JSON-представление элемента), поэтому одинаковый рендер даёт побайтово одинаковую запись. Если элемент не маршалится (например, NaN), сортировка возвращает ошибку и не меняет порядок; паники нет. Store проверяет только, что результат не пуст и валиден как JSON, и не импортирует `desired` (правило `.go-arch-lint.yml`): зависимость идёт через структурную типизацию интерфейса.

Статусы target: `Unknown` (до первого sync), `Synced`, `OutOfSync` (только из `Status`), `Progressing` (идёт sync или rollback), `Degraded` (ошибка Apply, Prune, Health или записи ревизии), `Blocked` (невалидный манифест, ошибка render, фабрика Runtime или Inspect вернули ошибку). Статус `Healthy` в M1 не используется: здоровье target — часть `Synced`/`Degraded`. Сообщение (`message`) последнего sync содержит этап и причину ошибки. Panic в Runtime или store переводит target в `Degraded` с этапом `run` и текстом «panic: …», panic не выходит наружу. Отмена вызывающего во время render классифицируется как `Blocked`. Известное ограничение: если сама запись `Degraded` не удалась, статус остаётся `Progressing`; ошибка видна вызывающему, восстановление — повторный sync или ручная правка статуса.

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

1. **Секреты приложений.** *Решено (2026-10-27):* в v0 только external Docker secrets; Vault/SOPS — после v0, отдельным решением.
2. **UI.** *Решено (2026-10-27):* CLI+API до M5, web UI в M5.
3. **Репозиторий для deploy.** *Решено ADR-0005:* отдельный deploy-репозиторий.
4. **Бэкап SQLite.** *Решено (2026-10-27):* `VACUUM INTO` по расписанию и внешняя копия вне ноды control plane до прода, см. отдельный issue в M1/ops.

## 11. Что дальше

- Создать deploy-репозиторий и определить его раскладку.
- Разложить M0 на issues в GitHub.
- Начать с `Runtime.Inspect` + нормализации compose: без этого diff не на что опереть.
