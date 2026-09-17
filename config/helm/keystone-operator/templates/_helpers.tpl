{{/*
SPDX-License-Identifier: AGPL-3.0-or-later

Common helpers for the keystone-operator chart.
*/}}

{{/*
Expand the name of the chart.
*/}}
{{- define "keystone-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a fully qualified app name. Truncated to 63 chars (DNS label limit).
*/}}
{{- define "keystone-operator.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Chart name + version as a label value.
*/}}
{{- define "keystone-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Standard labels applied to every rendered object.
*/}}
{{- define "keystone-operator.labels" -}}
helm.sh/chart: {{ include "keystone-operator.chart" . }}
{{ include "keystone-operator.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: keystone
app.kubernetes.io/component: operator
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
Selector labels (subset of labels that is stable across upgrades).
*/}}
{{- define "keystone-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "keystone-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Service account name.
*/}}
{{- define "keystone-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "keystone-operator.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Image reference — repository + tag (defaults to Chart.appVersion).
*/}}
{{- define "keystone-operator.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end }}

{{/*
Webhook Service name (also the CN / DNS SAN on the Certificate).
*/}}
{{- define "keystone-operator.webhookServiceName" -}}
{{ include "keystone-operator.fullname" . }}-webhook
{{- end }}

{{/*
Metrics Service name.
*/}}
{{- define "keystone-operator.metricsServiceName" -}}
{{ include "keystone-operator.fullname" . }}-metrics
{{- end }}

{{/*
Webhook serving Certificate name.
*/}}
{{- define "keystone-operator.webhookCertName" -}}
{{ include "keystone-operator.fullname" . }}-webhook-serving-cert
{{- end }}

{{/*
Webhook serving Secret name (cert-manager writes the cert here).
*/}}
{{- define "keystone-operator.webhookSecretName" -}}
{{ include "keystone-operator.fullname" . }}-webhook-serving-cert
{{- end }}
