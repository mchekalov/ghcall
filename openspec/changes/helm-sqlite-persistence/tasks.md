# Tasks

## 1. Values and schema

- [x] 1.1 Add a commented `persistence` block to `deploy/helm/ghcall/values.yaml` (`enabled: true`, `existingClaim: ""`, `storageClass: ""`, `accessModes: [ReadWriteOnce]`, `size: 1Gi`, `annotations: {}`, `keepOnUninstall: true`). Note in the comments that it applies only when `config.cache.driver` is sqlite. Verify with `helm lint deploy/helm/ghcall`.
- [x] 1.2 Add `persistence` to `values.schema.json`: types, `accessModes` items enum `ReadWriteOnce|ReadWriteOncePod|ReadWriteMany`, and a `size` quantity pattern. Verify that `helm lint --set persistence.size=abc` fails and the default values pass.

## 2. Template helpers

- [x] 2.1 Add helpers to `_helpers.tpl`: `ghcall.cacheDriver` (dig with default `sqlite`), `ghcall.sqlitePersistent`, and `ghcall.cacheClaimName` (`existingClaim` or `<fullname>-cache`). Verify with `helm template` output in task 3.x.
- [x] 2.2 Add `ghcall.validate` to `_helpers.tpl`. It fails when the sqlite `cache.path` is set but not under `/var/cache/ghcall/`, and when `ghcall.sqlitePersistent` is true with `concurrencyPolicy` other than `Forbid`. Include it from `cronjob.yaml`. Verify that `helm template --set config.cache.driver=sqlite --set concurrencyPolicy=Replace` fails with a message naming `concurrencyPolicy`, and that `--set config.cache.path=/tmp/x.db` fails naming `/var/cache/ghcall/`.
- [x] 2.3 Switch the existing `$wantDSN` in `cronjob.yaml` to the `ghcall.cacheDriver` helper, so all driver checks use the same logic. Verify that postgres renders are unchanged (see 4.1).
- [x] 2.4 Route every `persistence.*` read through a `ghcall.persistence` helper that fills in the values.yaml defaults key by key. `helm upgrade --reuse-values` from 0.1.0 reuses values that have no `persistence` block, and without the helper the render failed with a nil pointer. Verify that `helm template --set persistence=null` still renders the default PVC and that `persistence.enabled=false` is still honoured. (Found during 6.1.)

## 3. Templates

- [x] 3.1 Create `templates/pvc.yaml`. It renders only when `ghcall.sqlitePersistent` is true and `existingClaim` is empty, with labels, `persistence.annotations`, the `helm.sh/resource-policy: keep` annotation when `keepOnUninstall`, `accessModes`, the requested `size`, and the `storageClass` rules (empty → omitted, `"-"` → `""`). Verify that `helm template --set config.cache.driver=sqlite` shows a PVC named `ghcall-cache` requesting 1Gi, and that `--set persistence.storageClass=-` renders `storageClassName: ""`.
- [x] 3.2 In `cronjob.yaml`, make the `state` volume a `persistentVolumeClaim` using `ghcall.cacheClaimName` when `ghcall.sqlitePersistent` is true, and an `emptyDir` otherwise. Rewrite the comment above the `state` mount (it currently says sqlite is ephemeral). Verify that the sqlite render shows `claimName: ghcall-cache`, the `--set persistence.existingClaim=foo` render shows `claimName: foo` with no PVC, and the `--set persistence.enabled=false` render shows `emptyDir`.
- [x] 3.3 In `configmap.yaml`, when the driver is sqlite, `deepCopy` `.Values.config`, default `cache.path` to `/var/cache/ghcall/cache.db`, then render with `omit "agent" | toYaml`. Verify that the sqlite render's `config.yaml` contains `path: /var/cache/ghcall/cache.db` and that a user-set `/var/cache/ghcall/prod.db` is kept.
- [x] 3.4 Update `NOTES.txt`. When the sqlite cache is persistent, print the claim name, the `kubectl delete pvc` hint when keep is on, the WaitForFirstConsumer/`--wait` caveat, and a first-upgrade re-report note. When the sqlite cache is ephemeral, print a warning that every run re-reports. Verify with `helm template` output (NOTES render under `helm install --dry-run`).

## 4. Regression and new render cases

- [x] 4.1 Before and after the template changes, render each existing `ci/*-values.yaml` (all postgres) and `diff` the outputs. Verify that the only differences are the chart version labels.
- [x] 4.2 Add `ci/sqlite-values.yaml` (sqlite driver, persistence with a custom size and storageClass, agent enabled). Verify with `helm lint -f` and `helm template -f ... | kubectl apply --dry-run=client -f -`.
- [x] 4.3 Confirm that no agent Job gets the volume: `grep` `internal/agent/kubernetes.go` for volume or PVC fields, and confirm there is no reference to the cache claim (no code change expected).

## 5. Docs and version

- [x] 5.1 Update the chart README Prerequisites so that it offers PostgreSQL or sqlite with a PVC, add a "SQLite cache" section (values, single-writer/Forbid rule, RWO node pinning, NFS caveat, `--wait` caveat, uninstall/keep), and mention the new render case under "Rendering without a cluster". Verify by reading the rendered markdown.
- [x] 5.2 Update the header comment in `configs/example-kubernetes.yaml` ("no PVC") so it says postgres is one choice and the chart also supports a persistent sqlite cache. Verify with `go test ./internal/config/...` if the example is used in tests; otherwise review it.
- [x] 5.3 Bump `deploy/helm/ghcall/Chart.yaml` `version` to `0.2.0`. Verify that `helm lint` passes.

## 6. End-to-end check (optional, needs a cluster)

- [x] 6.1 On a local cluster (Rancher Desktop), install with `config.cache.driver=sqlite`, run two manual Jobs with `kubectl create job --from=cronjob/...`, and confirm from the logs that the second run reports only changes (no full re-report) and the PVC is `Bound`.
