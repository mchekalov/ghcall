{{/* Chart name, overridable with nameOverride. */}}
{{- define "ghcall.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully-qualified release name. Kept under 63 characters because it prefixes
the CronJob name, and a CronJob's own name is further limited: the Jobs it
creates append "-<timestamp>".
*/}}
{{- define "ghcall.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 52 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 52 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "ghcall.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "ghcall.labels" -}}
helm.sh/chart: {{ include "ghcall.chart" . }}
{{ include "ghcall.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "ghcall.selectorLabels" -}}
app.kubernetes.io/name: {{ include "ghcall.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "ghcall.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "ghcall.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* Name of the Secret holding the GitHub token: an existing one, or the chart's own. */}}
{{- define "ghcall.githubSecretName" -}}
{{- default (printf "%s-credentials" (include "ghcall.fullname" .)) .Values.github.existingSecret.name -}}
{{- end -}}

{{- define "ghcall.githubSecretKey" -}}
{{- default "token" .Values.github.existingSecret.key -}}
{{- end -}}

{{/* Name of the Secret holding the GitLab PAT. Only referenced when some filter uses provider: gitlab. */}}
{{- define "ghcall.gitlabSecretName" -}}
{{- default (printf "%s-credentials" (include "ghcall.fullname" .)) .Values.gitlab.existingSecret.name -}}
{{- end -}}

{{- define "ghcall.gitlabSecretKey" -}}
{{- default "gitlab-token" .Values.gitlab.existingSecret.key -}}
{{- end -}}

{{/*
True when any filter targets GitHub (the default provider), i.e. when ghcall
needs a GitHub token. A GitLab-only install must not reference a GitHub
Secret it never created: the pod would sit in CreateContainerConfigError.
*/}}
{{- define "ghcall.usesGitHub" -}}
{{- range (dig "filters" (list) .Values.config) -}}
{{- if eq (default "github" .provider) "github" -}}true{{- end -}}
{{- end -}}
{{- end -}}

{{/* True when any filter targets GitLab, i.e. when ghcall needs a GitLab token. */}}
{{- define "ghcall.usesGitLab" -}}
{{- range (dig "filters" (list) .Values.config) -}}
{{- if eq (default "github" .provider) "gitlab" -}}true{{- end -}}
{{- end -}}
{{- end -}}

{{- define "ghcall.databaseSecretName" -}}
{{- default (printf "%s-credentials" (include "ghcall.fullname" .)) .Values.database.existingSecret.name -}}
{{- end -}}

{{- define "ghcall.databaseSecretKey" -}}
{{- default "dsn" .Values.database.existingSecret.key -}}
{{- end -}}

{{/* True when the chart must render its own Secret from inline values. */}}
{{- define "ghcall.createSecret" -}}
{{- if or
      (and .Values.github.token (not .Values.github.existingSecret.name))
      (and .Values.gitlab.token (not .Values.gitlab.existingSecret.name))
      (and .Values.database.dsn (not .Values.database.existingSecret.name)) -}}
true
{{- end -}}
{{- end -}}

{{/*
The cache driver ghcall will actually use. An unset or empty driver is
sqlite, matching ghcall's own default, so every driver check in the chart
goes through here.
*/}}
{{- define "ghcall.cacheDriver" -}}
{{- default "sqlite" (dig "cache" "driver" "" .Values.config) -}}
{{- end -}}

{{/*
.Values.persistence with values.yaml's defaults filled in, as YAML (read it
back with fromYaml). `helm upgrade --reuse-values` from a chart older than
0.2.0 reuses the old release's values, which have no persistence block at
all. Keys are filled per missing key rather than with merge, which would
also overwrite an explicit false.
*/}}
{{- define "ghcall.persistence" -}}
{{- $p := deepCopy (default (dict) .Values.persistence) -}}
{{- $defaults := dict "enabled" true "existingClaim" "" "storageClass" "" "accessModes" (list "ReadWriteOnce") "size" "1Gi" "annotations" (dict) "keepOnUninstall" true -}}
{{- range $k, $v := $defaults -}}
{{- if not (hasKey $p $k) -}}{{- $_ := set $p $k $v -}}{{- end -}}
{{- end -}}
{{- toYaml $p -}}
{{- end -}}

{{/* True when the sqlite cache lives on a PVC rather than the emptyDir. */}}
{{- define "ghcall.sqlitePersistent" -}}
{{- if and (eq (include "ghcall.cacheDriver" .) "sqlite") (include "ghcall.persistence" . | fromYaml).enabled -}}true{{- end -}}
{{- end -}}

{{/* The PVC holding the sqlite cache: an existing one, or the chart's own. */}}
{{- define "ghcall.cacheClaimName" -}}
{{- default (printf "%s-cache" (include "ghcall.fullname" .)) (include "ghcall.persistence" . | fromYaml).existingClaim -}}
{{- end -}}

{{/*
Render-time checks for combinations the schema cannot express. Each would
otherwise surface only at runtime, as a crash-looping or corrupting pod.
*/}}
{{- define "ghcall.validate" -}}
{{- if eq (include "ghcall.cacheDriver" .) "sqlite" -}}
{{- $path := dig "cache" "path" "" .Values.config -}}
{{- if and $path (or (not (hasPrefix "/var/cache/ghcall/" $path)) (contains ".." $path)) -}}
{{- fail (printf "config.cache.path %q: the sqlite cache must be under /var/cache/ghcall/, the only writable path in the pod (the root filesystem is read-only). Leave it empty for /var/cache/ghcall/cache.db." $path) -}}
{{- end -}}
{{- end -}}
{{- if and (include "ghcall.sqlitePersistent" .) (ne .Values.concurrencyPolicy "Forbid") -}}
{{- fail (printf "concurrencyPolicy %q: a persistent sqlite cache requires Forbid. At most one ghcall pod may hold the cache at a time: SQLite has a single writer, and a ReadWriteOnce volume attaches to one node." .Values.concurrencyPolicy) -}}
{{- end -}}
{{- end -}}
