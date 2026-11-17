{{- define "whatchanged.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "whatchanged.fullname" -}}
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

{{- define "whatchanged.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "whatchanged.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "whatchanged.selectorLabels" -}}
app.kubernetes.io/name: {{ include "whatchanged.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "whatchanged.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "whatchanged.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "whatchanged.ingestSecretName" -}}
{{- default (printf "%s-ingest" (include "whatchanged.fullname" .)) .Values.ingest.existingSecret }}
{{- end }}

{{/* Whether a tracker runs, mirroring the binary's defaults. */}}
{{- define "whatchanged.trackerOn" -}}
{{- $name := index . 0 -}}
{{- $default := index . 1 -}}
{{- $v := index . 2 -}}
{{- if has $name $v.trackers.disable -}}
{{- else if or $default (has $name $v.trackers.enable) -}}true
{{- end -}}
{{- end }}
