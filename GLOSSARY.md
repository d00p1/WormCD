# Glossary

Термины WormCD. Используются в коде, issue и документации; синонимы не приветствуются.

## Application

Приложение, описанное в git файлом `wormcd.yaml` и compose-файлом. Идентифицируется по `metadata.name`, глобально уникально в пределах control plane.

## Target

Конкретная пара «приложение + окружение + кластер», в которую выполняется sync. Полное имя — `app/target`, например `web/prod`. Один Application может иметь несколько Target.

## Cluster

Docker Swarm (позже — Docker) кластер, зарегистрированный в control plane. Хранит endpoint и credentials. Не путать с Target: один Cluster обслуживает много Target.

## Environment / Overlay

Environment — логическое окружение (`staging`, `prod`). Overlay — compose override-файл, который накладывается на базовый `compose.yaml` для этого окружения. Overlay не является отдельным языком.

## Desired state

Состояние, полученное после рендера `compose.yaml + overlay`, нормализованное в модель `Desired`. Единственный источник — git.

## Observed state

Фактическое состояние кластера, полученное через `Runtime.Inspect` и отфильтрованное по меткам приложения.

## Drift

Расхождение между desired и observed, возникшее вне WormCD (ручное изменение, падение, чужой деплой). Устраняется sync-ом при `selfHeal: true`.

## Sync

Процесс приведения observed к desired: diff → apply → health → запись ревизии. Может быть автоматическим (по коммиту/интервалу) или ручным.

## Plan

Упорядоченный результат diff между Desired и Observed: для каждого объекта `kind/name` действие `Create`, `Update`, `Delete` или `Noop`. Plan строится read-only и служит входом для apply.

## Revision

Запись об успешном (или завершившемся ошибкой) sync: commit, полностью отрендеренный манифест, digest-ы образов. Основа для истории, diff-а и rollback.

## Runtime

Интерфейс, через который ядро применяет состояние. Реализации: `swarm`, позже `docker`. Ядро не содержит знаний о конкретном рантайме.

## Control plane

Процесс `wormcd server`: API, git-кэш, планировщик, reconciler, хранилище. Один control plane обслуживает N Cluster.

## Source

Зарегистрированный git-репозиторий (URL + ветка + путь), в котором control plane ищет `wormcd.yaml`. Source не хранит состояние приложений, только адрес.

## Stack namespace

Namespace объектов в Swarm, соответствующий Target. Все объекты помечаются `com.wormcd.application` и `com.wormcd.target`, чтобы diff и prune не задевали чужие ресурсы.
