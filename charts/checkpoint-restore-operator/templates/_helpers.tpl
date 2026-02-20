{{- define "cro.name" -}}
checkpoint-restore-operator
{{- end -}}

{{- define "cro.fullname" -}}
{{- if .Release.Name -}}
{{- printf "%s-%s" .Release.Name (include "cro.name" .) | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- include "cro.name" . -}}
{{- end -}}
{{- end -}}

{{- define "cro.saName" -}}
{{- if .Values.serviceAccount.name -}}
{{ .Values.serviceAccount.name }}
{{- else -}}
{{ include "cro.fullname" . }}
{{- end -}}
{{- end -}}

