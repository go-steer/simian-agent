{{/*
Common labels for all simian-controller resources.
*/}}
{{- define "simian.labels" -}}
app.kubernetes.io/name: simian-agent
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{/*
Selector labels for the controller.
*/}}
{{- define "simian.selectorLabels" -}}
app.kubernetes.io/name: simian-agent
app.kubernetes.io/component: controller
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
