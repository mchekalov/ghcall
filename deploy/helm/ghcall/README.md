# ghcall Helm chart

Runs [ghcall](https://github.com/mchekalov/ghcall) in Kubernetes as a
`CronJob`, optionally letting it trigger the AI CI-autofix agent as a
Kubernetes `Job` per failing-CI PR.

## Why a CronJob

ghcall is a one-shot CLI: one incremental pass, then exit. There is no
server, no port, no health endpoint — so there is no Deployment, Service or
probe in this chart, and nothing to template for them. The schedule is the
whole runtime model.

## Prerequisites

- A PostgreSQL database. The chart does **not** deploy one; it takes a DSN
  from a Secret. Any reachable Postgres works, in-namespace or managed.
  (`config.cache.driver: sqlite` also works, but the file lives on an
  `emptyDir` and dies with the pod, so every run re-reports everything.)
- For GitHub filters (the default provider): a GitHub token in a Secret. A
  GitLab-only install needs none; the pod gets no `GITHUB_TOKEN` at all.
- For GitLab filters: a self-hosted GitLab reachable from the cluster and a
  read-only PAT (`read_api` scope) in a Secret.
- If the agent is enabled: the agent image in a reachable registry, and a
  Secret holding its model credentials.

## Install

```bash
kubectl create secret generic ghcall-github --from-literal=token="$GITHUB_TOKEN"
kubectl create secret generic ghcall-db --from-literal=dsn="postgres://ghcall:pw@postgres:5432/ghcall?sslmode=require"

helm install ghcall deploy/helm/ghcall \
  --set github.existingSecret.name=ghcall-github \
  --set database.existingSecret.name=ghcall-db \
  --set 'config.filters[0].name=ci-watch' \
  --set 'config.filters[0].repos[0]=myorg/myrepo'
```

`github.token` / `database.dsn` can be set inline instead, but those values
land in Helm's release history in plain text — the chart prints a warning
when you do. Use them for a trial, not a deployment.

The agent is off by default, so a first install is detect-and-print only:

```bash
kubectl create job --from=cronjob/ghcall ghcall-manual-1
kubectl logs job/ghcall-manual-1
```

Once the JSON looks right, `--set agent.enabled=true`.

## ghcall's own config

`config:` is ghcall's config file as a values tree. It is rendered with
`toYaml` into a ConfigMap and mounted read-only at `/etc/ghcall/config.yaml`,
so anything `configs/example.yaml` documents is valid there. A
`checksum/config` pod annotation makes the next scheduled Job pick up edits.

### GitLab filters

A filter with `provider: gitlab` is polled from the same pass as the GitHub
ones. Set `config.gitlab.base_url` (the instance root, not its `/api` path)
and point `gitlab.existingSecret` at a Secret holding the PAT; the chart only
puts `GITLAB_TOKEN` on the pod when some filter actually asks for GitLab.

```bash
kubectl create secret generic ghcall-gitlab --from-literal=gitlab-token="$GITLAB_TOKEN"

helm upgrade ghcall deploy/helm/ghcall \
  --set gitlab.existingSecret.name=ghcall-gitlab \
  --set config.gitlab.base_url=https://git.example.com \
  --set 'config.filters[1].name=renovate-mrs' \
  --set 'config.filters[1].provider=gitlab' \
  --set 'config.filters[1].repos[0]=group/subgroup/project'
```

Nested namespaces are supported: the last path segment is the project, and
everything before it the namespace.

GitLab support is **detection only**. ghcall reports the MRs and tracks their
pipeline status; it never approves, merges, rebases or comments — that is the
agent container's job — and it never sends a GitLab MR to the agent. The
container's `--platform gitlab` mode reads `CI_PROJECT_ID` and
`CI_MERGE_REQUEST_IID` from the ambient pipeline environment, so it can only
be driven from inside a failing GitLab pipeline, not from outside by ghcall.
Each run logs how many GitLab candidates it skipped; that count is the
measure of what teaching the container an outside-in mode would buy.

The one exception is `config.agent`, which is **ignored**: the agent block is
built from the top-level `agent:` values instead, so enabling the agent also
wires the `kubernetes` launcher and its RBAC from a single switch.

## The agent launcher

With `agent.enabled=true`, ghcall creates one Job per PR it reports with a
failing CI state (`FAILURE`/`ERROR`) instead of shelling out to `docker run`.
No docker socket is mounted, and nothing requires a Docker-shim node.

The Job's name is derived from the repo and PR number:
`ghcall-autofix-<repo>-<pr>`. That determinism **is** the dedupe mechanism —
a second ghcall run that sees the same red PR POSTs the same name and gets a
`409 AlreadyExists`, which is logged and skipped. Idempotency state lives in
the API server, not in ghcall's cache.

```bash
kubectl get jobs -l app.kubernetes.io/name=ghcall-autofix
kubectl logs job/ghcall-autofix-myorg-myrepo-42
```

### Tuning the dedupe window

The window is `agent.kubernetes.jobTTLSeconds`, because a finished Job keeps
blocking its own name until `ttlSecondsAfterFinished` garbage-collects it.

- Too high, and a PR that fails again is not re-triggered for that long.
- Too low, and a flapping PR gets re-triggered every schedule tick.

3600s against the default 10-minute schedule is a reasonable start: a red PR
gets at most one autofix attempt per hour.

`backoffLimit: 0` on the agent Job is deliberate — a failed autofix run is
not retried by the API server. The next ghcall run re-reads the PR's CI state
and decides again.

### Agent credentials

`agent.kubernetes.envFromSecrets` mounts the agent's Bedrock/LiteLLM
credentials onto the *agent Job*, so they never have to exist in ghcall's own
pod. That is why `config.agent.env_passthrough` (the docker launcher's
mechanism) stays empty in Kubernetes.

### RBAC

The Role the chart creates is exactly:

```yaml
rules:
  - apiGroups: ["batch"]
    resources: ["jobs"]
    verbs: ["create"]
```

The launcher only ever POSTs a Job — it never lists, watches, reads or
deletes — so nothing more is needed. Set `rbac.create=false` to manage it
yourself. If `agent.kubernetes.namespace` points at a different namespace
from the release, create the Role and RoleBinding there yourself; the chart's
RoleBinding subject stays in the release namespace.

## Required egress

| From | To | Why |
|---|---|---|
| ghcall pod | `api.github.com:443` | REST change-detection and GraphQL PR fetches |
| ghcall pod | the GitLab instance (443) | project checks and MR/pipeline GraphQL queries |
| ghcall pod | the Postgres service | cache |
| ghcall pod | the in-cluster API server | creating agent Jobs |
| nodes | `artifactorycn.netcracker.com:17008` | pulling the ghcall and agent images |
| agent Jobs | `api.github.com:443` | pushing commits, comments, merges |
| agent Jobs | the Bedrock / LiteLLM gateway | model calls |

## Values

See `values.yaml` for the full list with comments; `values.schema.json`
rejects the common misconfigurations at install time. The two files under
`ci/` are worked examples: `existing-secrets-values.yaml` is the production
shape, `agent-enabled-values.yaml` exercises every optional branch.

## Upgrading

The cache tables are keyed by provider, so that `github:foo/bar` and
`gitlab:foo/bar` cannot collide. That changed their primary key, which no
`CREATE TABLE IF NOT EXISTS` can migrate in place: on the first run after the
upgrade, ghcall drops and recreates them.

This is a pure cache, so the only cost is one run's worth of re-reporting —
but with the agent enabled, "re-report every matching PR" means a burst of
autofix Jobs. Run the first pass with `agent.enabled=false`, or accept the
burst knowingly. `helm install`/`upgrade` prints the same warning.

## Rendering without a cluster

```bash
helm lint deploy/helm/ghcall
helm template ghcall deploy/helm/ghcall -f deploy/helm/ghcall/ci/agent-enabled-values.yaml \
  | kubectl apply --dry-run=client -f -
```
