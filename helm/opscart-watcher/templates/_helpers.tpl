{{/*
Expand the name of the chart.
*/}}
{{- define "opscart-watcher.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "opscart-watcher.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "opscart-watcher.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
app.kubernetes.io/name: {{ include "opscart-watcher.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "opscart-watcher.selectorLabels" -}}
app.kubernetes.io/name: {{ include "opscart-watcher.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Namespace
*/}}
{{- define "opscart-watcher.namespace" -}}
{{- .Release.Namespace }}
{{- end }}

{{/*
ServiceAccount name
*/}}
{{- define "opscart-watcher.serviceAccountName" -}}
{{- .Values.serviceAccount.name }}
{{- end }}

{{/*
PVC claim name — the templated PVC's name, or the user-supplied existingClaim.
*/}}
{{- define "opscart-watcher.pvcName" -}}
{{- if .Values.persistence.existingClaim }}
{{- .Values.persistence.existingClaim }}
{{- else }}
{{- include "opscart-watcher.fullname" . }}
{{- end }}
{{- end }}

{{/*
Auth Secret name — preserve an explicitly supplied Secret; otherwise use a
release-managed Secret whose data is retained with lookup on upgrades.
*/}}
{{- define "opscart-watcher.authSecretName" -}}
{{- if .Values.auth.existingSecret }}
{{- .Values.auth.existingSecret }}
{{- else }}
{{- printf "%s-auth" (include "opscart-watcher.fullname" .) }}
{{- end }}
{{- end }}
