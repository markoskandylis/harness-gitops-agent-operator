{{- define "harness-gitops-agent-bootstrap.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "harness-gitops-agent-bootstrap.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name (include "harness-gitops-agent-bootstrap.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{- define "harness-gitops-agent-bootstrap.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "harness-gitops-agent-bootstrap.labels" -}}
helm.sh/chart: {{ include "harness-gitops-agent-bootstrap.chart" . }}
{{ include "harness-gitops-agent-bootstrap.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "harness-gitops-agent-bootstrap.selectorLabels" -}}
app.kubernetes.io/name: {{ include "harness-gitops-agent-bootstrap.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
