# Design

## Context

- ghcall runs as a CronJob. Every tick starts a new Job and pod, which runs one pass and exits. Anything the cache needs to keep between passes has to be stored outside the pod.
- `cronjob.yaml` already mounts a volume named `state` at `/var/cache/ghcall`. It is an `emptyDir` today and is the only writable path, because `readOnlyRootFilesystem: true`. `podSecurityContext` already sets `fsGroup: 65532`, so a mounted PVC is writable by the distroless `nonroot` user without an init container.
- `internal/cache/sqlite.go` runs `MkdirAll` on the path's parent directory and pins `SetMaxOpenConns(1)`. `internal/config` expands `~/` and otherwise uses `cache.path` as given. No Go changes are needed.
- The ConfigMap is rendered as `omit .Values.config "agent" | toYaml`, so what the user writes under `config` goes through verbatim. The pod template carries a `checksum/config` annotation, so a change to the injected path rolls out on the next tick.
- The chart's sqlite detection has to match ghcall's own default. `cronjob.yaml` already computes `$wantDSN` with `dig "cache" "driver" "sqlite"`, so an unset driver counts as sqlite.
- CI (`.github/workflows/ci.yml`) runs only `go build` and `go test`. The chart is checked by hand with `helm lint` and `helm template`, as the chart README describes. The chart has no unittest plugin.

## Goals / Non-Goals

**Goals:**
- A sqlite install that works with no extra settings beyond `config.cache.driver: sqlite`.
- Reject unsafe combinations at render time, rather than letting them corrupt data or hang the pod at runtime.
- Postgres output stays byte-identical to what the chart renders today.

**Non-Goals:**
- Backups, snapshots, or migrating cache contents between sqlite and postgres. The cache is disposable, so losing it costs one re-report pass.
- A StatefulSet or long-running Deployment mode.
- Volumes on agent Jobs.
- Changing the chart's default driver away from `postgres`.
- Automated chart tests in CI. That would be a separate change.

## Decisions

### D1. Replace the source of the existing `state` volume instead of adding a second volume
When the cache is persistent, the `state` volume becomes a `persistentVolumeClaim`; otherwise it stays `emptyDir`. The mount path and name stay the same, so the `/var/cache/ghcall` contract doesn't change, and there is still exactly one writable path.
*Alternative:* a separate `cache` volume at a new path. Rejected because it adds a second writable location and changes a path that users may already have in `config.cache.path`.

### D2. Gate on the driver as well as `persistence.enabled`, and default `enabled: true`
A new helper, `ghcall.sqlitePersistent`, is true when the driver is sqlite and `persistence.enabled` is true. The PVC template, the volume source, and the concurrency check all use this one helper. Defaulting to `true` means someone who switches only the driver to sqlite gets a working cache. Postgres users never see a PVC, even though `persistence.enabled` defaults to true, so their output is unchanged.
*Alternative:* default `enabled: false` (opt in). Rejected because ephemeral sqlite is exactly the broken state this change fixes, and making it the default would keep the pitfall.

### D3. Chart-owned PVC with `helm.sh/resource-policy: keep` by default; `existingClaim` escape hatch
The chart renders `templates/pvc.yaml`, named `<fullname>-cache`, following the usual Bitnami-style `persistence.*` values. `keepOnUninstall: true` adds the keep annotation, so `helm uninstall` followed by a reinstall doesn't cause a full re-report burst, which is costly when the agent is enabled. `existingClaim` covers statically provisioned volumes and GitOps setups that manage PVCs separately.
Storage class convention: an empty value leaves the field out (cluster default), and `"-"` renders `storageClassName: ""` (no dynamic provisioning).

### D4. Inject the default `cache.path` in the ConfigMap and validate a user-set one
ghcall's own default, `~/.cache/ghcall/cache.db`, resolves onto the read-only root filesystem, so the chart has to override it. In `configmap.yaml`, when the driver is sqlite, the chart takes a `deepCopy` of `.Values.config`, sets `cache.path` to `/var/cache/ghcall/cache.db` if it is empty, and then runs `omit "agent" | toYaml`. The validation helper fails the render if a set path doesn't start with `/var/cache/ghcall/`. This check applies even with persistence disabled, because the `emptyDir` is still the only writable path.
*Alternative:* always force the path and ignore user input. Rejected because users may want a different filename on a shared `existingClaim`.

### D5. Require `concurrencyPolicy: Forbid` when sqlite is persistent
- With `Allow`, two pods overlap. On different nodes, a ReadWriteOnce volume gives a Multi-Attach error. On the same node, two processes write to one SQLite file.
- With `Replace`, the controller deletes the running Job and immediately creates the next one. The old pod may still be inside its termination grace period, so the same two failures apply.

`Forbid` is already the default. The check lives in `ghcall.validate`, is included from `cronjob.yaml`, and uses `fail`, the same way the chart already turns misconfiguration into render errors.
*Alternative:* ReadWriteOncePod access mode instead. Rejected as the enforcement mechanism: it needs CSI support, and on its own it only makes the second pod fail to schedule. Users can still select it with `persistence.accessModes: [ReadWriteOncePod]`.

### D6. Schema enforcement for shape only; cross-field rules in templates
`values.schema.json` gets a `persistence` object (types, `accessModes` enum, `size` pattern). The cross-field checks (driver × policy, driver × path) are written with `fail` in templates, not with schema `if/then`. The error messages are clearer that way, and the defaulting logic (unset driver means sqlite) stays in one place.

## Risks / Trade-offs

- [`helm install --wait` hangs on a WaitForFirstConsumer StorageClass: the PVC stays Pending until the first scheduled Job pod consumes it, and Helm waits for PVCs to bind] → Document this in the README's SQLite section and NOTES.txt. Suggest either skipping `--wait` or triggering a first run with `kubectl create job --from=cronjob/...`.
- [A ReadWriteOnce volume ties every run to the node or zone where the volume is attached, which limits scheduling] → This is acceptable for a tiny cache file. Document it, and point users to `nodeSelector`/`affinity` or to postgres for multi-zone clusters.
- [A sqlite release already running on `emptyDir` gets a new PVC on upgrade. The first run after the upgrade starts empty and re-reports everything, which is the same behavior it has today on every run] → Add a NOTES.txt line. After that first run, re-reporting stops.
- [An orphaned PVC after uninstall, because of the keep policy] → This is intentional. Document `kubectl delete pvc <fullname>-cache` and the `keepOnUninstall: false` opt-out.
- [`helm upgrade --reuse-values` from 0.1.0 reuses the old release's values, which have no `persistence` block, so values.yaml defaults never apply] → Templates read persistence through a helper that fills in each missing key with its default, so upgrading an existing release works either way.
- [SQLite on network file systems (NFS, some RWX classes) has unreliable locking] → Default to ReadWriteOnce and document that RWX/NFS classes aren't recommended for the cache.

## Migration Plan

1. Bump the chart version (0.1.0 → 0.2.0). No Go or image change is needed.
2. Postgres releases: `helm upgrade` makes no rendered change apart from the chart label.
3. Sqlite releases: `helm upgrade` creates `<fullname>-cache`. The next tick writes the cache there, and later ticks are incremental. If the agent is enabled, the first tick can cause the usual re-report burst. NOTES.txt already warns about this pattern.
4. Rollback: `helm rollback` goes back to `emptyDir`. Because of the keep annotation, the PVC stays in place and is reused if the release upgrades again.
