# Proposal

## Why

The Helm chart supports `config.cache.driver: sqlite` only in name. The CronJob mounts an `emptyDir` at `/var/cache/ghcall`, so the cache file is lost when each run ends and every run re-reports every matching PR. With the agent enabled, that means a burst of autofix Jobs on every tick. On top of that, `cache.path` defaults to `~/.cache/ghcall/cache.db`, which sits on the read-only root filesystem, so a sqlite install without an explicit path fails at startup. Small installs that don't want to run a PostgreSQL server have no working option today.

## What Changes

- Add a `persistence` block to the chart's values: `enabled`, `existingClaim`, `storageClass`, `accessModes`, `size`, `annotations`, and `keepOnUninstall`.
- When the cache driver is `sqlite` and persistence is enabled, the chart renders a PersistentVolumeClaim, unless `existingClaim` names one. It then mounts that claim in the ghcall CronJob pod at `/var/cache/ghcall`, in place of the `emptyDir`.
- When the driver is `sqlite` and `config.cache.path` is unset, the chart writes `/var/cache/ghcall/cache.db` into the rendered config. If a `path` is set, it must be under `/var/cache/ghcall/`, or rendering fails.
- Rendering also fails for a persistent sqlite cache combined with any `concurrencyPolicy` other than `Forbid`. SQLite allows one writer at a time, and a ReadWriteOnce volume can only be mounted on one node.
- The `postgres` driver keeps its current behavior: no PVC and no persistence volume. The chart's default stays `postgres`.
- Agent Jobs created by the kubernetes launcher get no volume. The cache belongs to ghcall's own pod only.
- Update `values.schema.json`, the chart README (Prerequisites, a new "SQLite cache" section, network table), NOTES.txt, the `emptyDir` comment in `cronjob.yaml`, and add a `ci/sqlite-values.yaml` render case.
- Behavior change (not breaking): a release that already runs with `driver: sqlite` gets a new PVC on its next upgrade, because persistence defaults to enabled. Before this change, those releases lost the cache after every run anyway.

## Capabilities

### New Capabilities
- `helm-cache-persistence`: how the Helm chart provides durable storage for the sqlite cache backend. Covers PVC creation or reuse, mounting it in the CronJob pod, the default cache path and path validation, the guard against concurrent writers, and leaving postgres installs unchanged.

### Modified Capabilities
<!-- none: openspec/specs/ is empty; no existing capability covers the chart -->

## Impact

- **Chart templates**: `deploy/helm/ghcall/templates/cronjob.yaml` (volume source and comment), `configmap.yaml` (inject the default `cache.path`), new `pvc.yaml`, `_helpers.tpl` (sqlite and persistence helpers, claim name, validation), `NOTES.txt`.
- **Chart values**: `values.yaml` (new `persistence` block), `values.schema.json`, and a new `ci/sqlite-values.yaml`.
- **Docs**: `deploy/helm/ghcall/README.md` and the header comment in `configs/example-kubernetes.yaml`.
- **Go code**: no change. `internal/cache/sqlite.go` already runs `MkdirAll` on the path's directory and uses a single connection, and `internal/config` already accepts an absolute `cache.path`.
- **Cluster requirements**: sqlite installs need a StorageClass that can provision ReadWriteOnce volumes, or a claim created ahead of time. The `fsGroup: 65532` already in `podSecurityContext` keeps the volume writable by the non-root user.
- **Chart version**: bump `Chart.yaml` `version` (minor), because this adds a feature.
