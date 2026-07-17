{{/*
Expand the name of the chart.
*/}}
{{- define "checkpoint-restore-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "checkpoint-restore-operator.fullname" -}}
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
Create chart name and version as used by the chart label.
*/}}
{{- define "checkpoint-restore-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "checkpoint-restore-operator.labels" -}}
helm.sh/chart: {{ include "checkpoint-restore-operator.chart" . }}
{{ include "checkpoint-restore-operator.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "checkpoint-restore-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "checkpoint-restore-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Manager selector labels.
*/}}
{{- define "checkpoint-restore-operator.manager.selectorLabels" -}}
{{ include "checkpoint-restore-operator.selectorLabels" . }}
app.kubernetes.io/component: manager
control-plane: controller-manager
{{- with .Values.operator.selectorLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "checkpoint-restore-operator.fullnameWithSuffix" -}}
{{- $root := index . 0 -}}
{{- $suffix := index . 1 -}}
{{- $maxBaseLen := int (max 1 (sub 62 (len $suffix))) -}}
{{- $base := include "checkpoint-restore-operator.fullname" $root | trunc $maxBaseLen | trimSuffix "-" -}}
{{- printf "%s-%s" $base $suffix | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "checkpoint-restore-operator.manager.name" -}}
{{- include "checkpoint-restore-operator.fullnameWithSuffix" (list . "controller-manager") }}
{{- end }}

{{- define "checkpoint-restore-operator.metricsService.name" -}}
{{- include "checkpoint-restore-operator.fullnameWithSuffix" (list . "controller-manager-metrics-service") }}
{{- end }}

{{/*
The service account name is a security principal: the restore annotation
admission policy exempts exactly this identity. Never fall back to the
namespace "default" service account, which would hand the restore
annotation privilege to every pod using it.
*/}}
{{- define "checkpoint-restore-operator.serviceAccountName" -}}
{{- $defaultName := include "checkpoint-restore-operator.fullnameWithSuffix" (list . "controller-manager") }}
{{- if .Values.operator.serviceAccount.create }}
{{- default $defaultName .Values.operator.serviceAccount.name }}
{{- else }}
{{- required "operator.serviceAccount.name is required when operator.serviceAccount.create is false" .Values.operator.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "checkpoint-restore-operator.managerRole.name" -}}
{{- include "checkpoint-restore-operator.fullnameWithSuffix" (list . "manager-role") }}
{{- end }}

{{- define "checkpoint-restore-operator.managerRoleBinding.name" -}}
{{- include "checkpoint-restore-operator.fullnameWithSuffix" (list . "manager-rolebinding") }}
{{- end }}

{{- define "checkpoint-restore-operator.leaderElectionRole.name" -}}
{{- include "checkpoint-restore-operator.fullnameWithSuffix" (list . "leader-election-role") }}
{{- end }}

{{- define "checkpoint-restore-operator.leaderElectionRoleBinding.name" -}}
{{- include "checkpoint-restore-operator.fullnameWithSuffix" (list . "leader-election-rolebinding") }}
{{- end }}

{{- define "checkpoint-restore-operator.metricsAuthRole.name" -}}
{{- include "checkpoint-restore-operator.fullnameWithSuffix" (list . "metrics-auth-role") }}
{{- end }}

{{- define "checkpoint-restore-operator.metricsAuthRoleBinding.name" -}}
{{- include "checkpoint-restore-operator.fullnameWithSuffix" (list . "metrics-auth-rolebinding") }}
{{- end }}

{{- define "checkpoint-restore-operator.metricsReaderRole.name" -}}
{{- include "checkpoint-restore-operator.fullnameWithSuffix" (list . "metrics-reader") }}
{{- end }}

{{- define "checkpoint-restore-operator.admissionPolicy.name" -}}
{{- include "checkpoint-restore-operator.fullnameWithSuffix" (list . "protect-restore-annotations") }}
{{- end }}

{{- define "checkpoint-restore-operator.criProxy.name" -}}
{{- include "checkpoint-restore-operator.fullnameWithSuffix" (list . "cri-restore-proxy") }}
{{- end }}

{{- define "checkpoint-restore-operator.criProxy.selectorLabels" -}}
{{ include "checkpoint-restore-operator.selectorLabels" . }}
app.kubernetes.io/component: cri-proxy
{{- end }}

{{/*
Derive the container port from a bind address such as ":8443" or
"127.0.0.1:8080", so the port cannot drift from the address the manager
actually listens on.
*/}}
{{- define "checkpoint-restore-operator.portFromBindAddress" -}}
{{- splitList ":" (toString .) | last -}}
{{- end }}

{{/*
Build an image reference from (root, image values). The tag defaults to
v<appVersion>, which released charts set from the VERSION file. A chart
installed from the repository checkout still carries the 0.0.0
placeholder, and the derived tag v0.0.0 is never published, so fail at
template time instead of deploying a pod stuck in ImagePullBackOff.
*/}}
{{- define "checkpoint-restore-operator.imageRef" -}}
{{- $root := index . 0 -}}
{{- $image := index . 1 -}}
{{- $registry := $root.Values.global.image.registry | default $image.registry -}}
{{- $tag := $image.tag | default (printf "v%s" $root.Chart.AppVersion) -}}
{{- if eq $tag "v0.0.0" -}}
{{- fail "set the image tag (for example --set image.tag=latest): the chart carries the 0.0.0 placeholder appVersion when installed from the repository checkout" -}}
{{- end -}}
{{- if $registry -}}
{{- printf "%s/%s:%s" $registry $image.repository $tag -}}
{{- else -}}
{{- printf "%s:%s" $image.repository $tag -}}
{{- end -}}
{{- end }}

{{- define "checkpoint-restore-operator.image" -}}
{{- include "checkpoint-restore-operator.imageRef" (list . .Values.image) -}}
{{- end }}

{{- define "checkpoint-restore-operator.criProxy.image" -}}
{{- include "checkpoint-restore-operator.imageRef" (list . .Values.criProxy.image) -}}
{{- end }}

{{- define "checkpoint-restore-operator.checkpointSyncer.name" -}}
{{- include "checkpoint-restore-operator.fullnameWithSuffix" (list . "checkpoint-syncer") }}
{{- end }}

{{- define "checkpoint-restore-operator.checkpointSyncer.selectorLabels" -}}
{{ include "checkpoint-restore-operator.selectorLabels" . }}
app.kubernetes.io/component: checkpoint-syncer
{{- end }}

{{- define "checkpoint-restore-operator.checkpointSyncer.image" -}}
{{- include "checkpoint-restore-operator.imageRef" (list . .Values.checkpointSyncer.image) -}}
{{- end }}

{{- define "checkpoint-restore-operator.checkpointSyncerRole.name" -}}
{{- include "checkpoint-restore-operator.fullnameWithSuffix" (list . "checkpoint-syncer-role") }}
{{- end }}

{{- define "checkpoint-restore-operator.checkpointSyncerRoleBinding.name" -}}
{{- include "checkpoint-restore-operator.fullnameWithSuffix" (list . "checkpoint-syncer-rolebinding") }}
{{- end }}

{{- define "checkpoint-restore-operator.checkpointSyncerSecretReader.name" -}}
{{- include "checkpoint-restore-operator.fullnameWithSuffix" (list . "checkpoint-syncer-secret-reader") }}
{{- end }}
