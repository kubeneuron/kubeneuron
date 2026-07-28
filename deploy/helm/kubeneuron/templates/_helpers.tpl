{{- define "kubeneuron.labels" -}}
app.kubernetes.io/name: kube-neuron
app.kubernetes.io/component: operator
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/instance: {{ .Release.Name }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- with .Values.additionalLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "kubeneuron.selectorLabels" -}}
app.kubernetes.io/name: kube-neuron
app.kubernetes.io/component: operator
{{- end }}
