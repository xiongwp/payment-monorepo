{{/* helpers — 各模板共享 */}}

{{- define "biz.fullimage" -}}
{{- $reg := .global.imageRegistry -}}
{{- $name := .service.image -}}
{{- $tag := .service.imageTag | default .global.imageTag -}}
{{- if $reg -}}{{ $reg }}/{{ $name }}:{{ $tag }}{{- else -}}{{ $name }}:local{{- end -}}
{{- end -}}

{{- define "biz.serviceLabels" -}}
app: {{ .name }}
tier: business
chart: biz-stack
{{- end -}}

{{- define "biz.podAnnotations" -}}
{{- if .Values.global.prometheus.scrape -}}
prometheus.io/scrape: "true"
prometheus.io/port: "{{ .Values.global.prometheus.port }}"
prometheus.io/path: "{{ .Values.global.prometheus.path }}"
{{- end -}}
{{- end -}}

{{/* 合并 global.env + service.env */}}
{{- define "biz.envList" -}}
{{- range $k, $v := .global.env }}
- name: {{ $k }}
  value: {{ $v | quote }}
{{- end }}
{{- range $k, $v := .service.env }}
- name: {{ $k }}
  value: {{ $v | quote }}
{{- end }}
- name: INTERNAL_TOKEN
  valueFrom:
    secretKeyRef:
      name: biz-secrets
      key: INTERNAL_TOKEN
{{- end -}}
