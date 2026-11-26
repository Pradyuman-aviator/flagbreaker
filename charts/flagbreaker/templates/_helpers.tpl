{{- define "flagbreaker.name" -}}
{{- .Release.Name | trunc 50 | trimSuffix "-" -}}
{{- end -}}

{{- define "flagbreaker.labels" -}}
app.kubernetes.io/name: flagbreaker
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "flagbreaker.selectorLabels" -}}
app.kubernetes.io/name: flagbreaker
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "flagbreaker.secretName" -}}
{{- default (printf "%s-flagsmith" (include "flagbreaker.name" .)) .Values.flagsmith.existingSecret -}}
{{- end -}}
