{{/* 服务名 helper */}}
{{- define "pp.fullname" -}}
{{- .name -}}
{{- end -}}

{{/* 标准 labels */}}
{{- define "pp.labels" -}}
app.kubernetes.io/name: {{ .name }}
app.kubernetes.io/instance: {{ .release }}
app.kubernetes.io/version: {{ .version | quote }}
app.kubernetes.io/managed-by: helm
app.kubernetes.io/part-of: payment-platform
{{- end -}}

{{/* image 全名 */}}
{{- define "pp.image" -}}
{{ .global.image.registry }}/{{ .name }}:{{ default .global.image.tag .svc.tag }}
{{- end -}}
