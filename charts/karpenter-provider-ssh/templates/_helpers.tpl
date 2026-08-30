{{/* Chart name */}}
{{- define "karpenter-provider-ssh.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fully qualified release name */}}
{{- define "karpenter-provider-ssh.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Common labels */}}
{{- define "karpenter-provider-ssh.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: karpenter
{{ include "karpenter-provider-ssh.selectorLabels" . }}
{{- end -}}

{{/* Selector labels */}}
{{- define "karpenter-provider-ssh.selectorLabels" -}}
app.kubernetes.io/name: {{ include "karpenter-provider-ssh.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* ServiceAccount name */}}
{{- define "karpenter-provider-ssh.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "karpenter-provider-ssh.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* Pool namespace (SSHHost inventory + secrets) */}}
{{- define "karpenter-provider-ssh.poolNamespace" -}}
{{- default .Release.Namespace .Values.poolNamespace -}}
{{- end -}}

{{/* Controller image ref */}}
{{- define "karpenter-provider-ssh.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (default (printf "v%s" .Chart.AppVersion) .Values.image.tag) -}}
{{- end -}}
{{- end -}}
