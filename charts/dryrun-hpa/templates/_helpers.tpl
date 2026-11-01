{{- define "dryrun-hpa.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "dryrun-hpa.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-controller" .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{- define "dryrun-hpa.selectorLabels" -}}
app.kubernetes.io/name: {{ include "dryrun-hpa.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "dryrun-hpa.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "dryrun-hpa.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "dryrun-hpa.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "dryrun-hpa.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "dryrun-hpa.claimName" -}}
{{- default (printf "%s-decisions" (include "dryrun-hpa.fullname" .)) .Values.store.persistence.existingClaim }}
{{- end }}
