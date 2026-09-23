# Spec Delta

## Purpose

Defines how the ghcall Helm chart gives the sqlite cache backend durable storage in Kubernetes, so change-detection and watched-PR state survive between CronJob runs without an external PostgreSQL server.

## ADDED Requirements

### Requirement: Persistent volume for the sqlite cache
When `config.cache.driver` is `sqlite` and `persistence.enabled` is true, the chart SHALL mount a PersistentVolumeClaim at `/var/cache/ghcall` in the ghcall CronJob pod, in place of the `emptyDir`. When `persistence.enabled` is false, the chart SHALL mount an `emptyDir` there, as it does today. `persistence.enabled` SHALL default to true.

#### Scenario: Default sqlite install
- **WHEN** the chart is rendered with `config.cache.driver: sqlite` and default `persistence` values
- **THEN** the output contains a PersistentVolumeClaim named `<fullname>-cache`
- **AND** the CronJob pod's `state` volume references that claim instead of an `emptyDir`
- **AND** the ghcall container mounts it read-write at `/var/cache/ghcall`

#### Scenario: Persistence explicitly disabled
- **WHEN** the chart is rendered with `config.cache.driver: sqlite` and `persistence.enabled: false`
- **THEN** no PersistentVolumeClaim is rendered
- **AND** the `state` volume is an `emptyDir`

#### Scenario: Cache survives between runs
- **WHEN** one scheduled Job completes a pass with the sqlite cache on a persistent claim, and the next scheduled Job starts
- **THEN** the next Job opens the same cache file and reports only PRs that changed since the previous pass

### Requirement: Claim provisioning is configurable
The chart SHALL render its own PersistentVolumeClaim using `persistence.size` (default `1Gi`), `persistence.accessModes` (default `[ReadWriteOnce]`), `persistence.storageClass`, and `persistence.annotations`. An empty `storageClass` SHALL leave `storageClassName` out, so the cluster default is used. The value `"-"` SHALL render `storageClassName: ""`, which disables dynamic provisioning. When `persistence.existingClaim` is set, the chart SHALL NOT render a claim and SHALL mount the named claim.

#### Scenario: Custom size and storage class
- **WHEN** the chart is rendered with sqlite, `persistence.size: 5Gi` and `persistence.storageClass: fast-ssd`
- **THEN** the rendered claim requests `5Gi` and sets `storageClassName: fast-ssd`

#### Scenario: Pre-existing claim
- **WHEN** the chart is rendered with sqlite and `persistence.existingClaim: my-ghcall-cache`
- **THEN** no PersistentVolumeClaim is rendered
- **AND** the `state` volume references claim `my-ghcall-cache`

### Requirement: Cache survives uninstall by default
The chart-rendered claim SHALL carry the annotation `helm.sh/resource-policy: keep` when `persistence.keepOnUninstall` is true. That value SHALL default to true.

#### Scenario: Uninstall keeps the cache
- **WHEN** a release with a chart-rendered cache claim is uninstalled while `persistence.keepOnUninstall` is true
- **THEN** the PersistentVolumeClaim is not deleted

#### Scenario: Opt out of keeping the claim
- **WHEN** the chart is rendered with `persistence.keepOnUninstall: false`
- **THEN** the claim has no `helm.sh/resource-policy` annotation

### Requirement: Sqlite cache path defaults to the mounted volume
When `config.cache.driver` is `sqlite` and `config.cache.path` is empty or unset, the rendered ghcall config SHALL set `cache.path` to `/var/cache/ghcall/cache.db`. When `config.cache.path` is set, it SHALL be an absolute path under `/var/cache/ghcall/`, or rendering SHALL fail with an error that names the allowed prefix.

#### Scenario: Path not set
- **WHEN** the chart is rendered with `config.cache.driver: sqlite` and no `config.cache.path`
- **THEN** the ConfigMap's `config.yaml` contains `cache.path: /var/cache/ghcall/cache.db`

#### Scenario: Custom path under the mount
- **WHEN** the chart is rendered with `config.cache.path: /var/cache/ghcall/prod.db`
- **THEN** the ConfigMap keeps that path unchanged

#### Scenario: Path outside the mount
- **WHEN** the chart is rendered with sqlite and `config.cache.path: ~/.cache/ghcall/cache.db`
- **THEN** rendering fails with a message saying the sqlite path must be under `/var/cache/ghcall/`

### Requirement: Single writer on a persistent sqlite cache
When the sqlite cache is persistent (driver `sqlite` and `persistence.enabled` true), the chart SHALL refuse to render unless `concurrencyPolicy` is `Forbid`. The error SHALL explain that at most one ghcall pod may hold the cache at a time.

#### Scenario: Replace policy rejected
- **WHEN** the chart is rendered with sqlite, persistence enabled, and `concurrencyPolicy: Replace`
- **THEN** rendering fails with an error naming `concurrencyPolicy` and requiring `Forbid`

#### Scenario: Ephemeral sqlite is not constrained
- **WHEN** the chart is rendered with sqlite, `persistence.enabled: false`, and `concurrencyPolicy: Allow`
- **THEN** rendering succeeds

### Requirement: Postgres installs are unaffected
When `config.cache.driver` is `postgres`, the chart SHALL NOT render a PersistentVolumeClaim, whatever the `persistence` values are. The `state` volume SHALL stay an `emptyDir`, and the rendered config SHALL contain no injected `cache.path`.

#### Scenario: Postgres with persistence values present
- **WHEN** the chart is rendered with `config.cache.driver: postgres` and `persistence.enabled: true`
- **THEN** no PersistentVolumeClaim is rendered
- **AND** the CronJob output matches what the chart rendered before this change

### Requirement: Agent Jobs do not mount the cache
Agent Jobs created by the kubernetes launcher SHALL NOT mount the cache volume or reference the cache claim.

#### Scenario: Agent enabled with persistent sqlite
- **WHEN** the chart is installed with sqlite, persistence enabled, and `agent.enabled: true`, and ghcall creates an autofix Job
- **THEN** that Job's pod spec contains no reference to the cache PersistentVolumeClaim
