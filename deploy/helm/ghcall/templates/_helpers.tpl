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

{{- define "ghcall.databaseSecretName" -}}
{{- default (printf "%s-credentials" (include "ghcall.fullname" .)) .Values.database.existingSecret.name -}}
{{- end -}}

{{- define "ghcall.databaseSecretKey" -}}
{{- default "dsn" .Values.database.existingSecret.key -}}
{{- end -}}

{{/* True when the chart must render its own Secret from inline values. */}}
{{- define "ghcall.createSecret" -}}
{{- if or (and .Values.github.token (not .Values.github.existingSecret.name)) (and .Values.database.dsn (not .Values.database.existingSecret.name)) -}}
true
{{- end -}}
{{- end -}}
