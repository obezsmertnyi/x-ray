{{/*
xray container args — a plain list driven entirely from .Values.args.
No conditionals: add/remove/reorder flags by editing values.yaml.
*/}}
{{- define "xray.containerArgs" -}}
{{- toYaml .Values.args }}
{{- if .Values.dev }}
- --dev
{{- end }}
{{- end -}}

{{/*
xray container env, generated from values.
*/}}
{{- define "xray.containerEnv" -}}
- name: POD_NAMESPACE
  valueFrom:
    fieldRef:
      fieldPath: metadata.namespace
{{- if .Values.github.secretName }}
- name: GITHUB_CLIENT_ID
  valueFrom:
    secretKeyRef:
      name: {{ .Values.github.secretName }}
      key: GITHUB_CLIENT_ID
- name: GITHUB_CLIENT_SECRET
  valueFrom:
    secretKeyRef:
      name: {{ .Values.github.secretName }}
      key: GITHUB_CLIENT_SECRET
{{- end }}
{{- if .Values.azure.secretName }}
- name: AZURE_CLIENT_ID
  valueFrom:
    secretKeyRef:
      name: {{ .Values.azure.secretName }}
      key: AZURE_CLIENT_ID
- name: AZURE_CLIENT_SECRET
  valueFrom:
    secretKeyRef:
      name: {{ .Values.azure.secretName }}
      key: AZURE_CLIENT_SECRET
- name: AZURE_TENANT_ID
  valueFrom:
    secretKeyRef:
      name: {{ .Values.azure.secretName }}
      key: AZURE_TENANT_ID
{{- end }}
{{- end -}}

{{/*
Container ports for the xray container, generated from values.
- http: always
- mcp (8083): only when MCP is enabled AND not served over OAuth on the main port
- metrics: only when metrics are enabled
*/}}
{{- define "xray.containerPorts" -}}
- name: http
  containerPort: {{ .Values.service.targetPort }}
{{- if and .Values.mcp.enabled (not .Values.mcp.oauth.enabled) }}
- name: mcp
  containerPort: 8083
{{- end }}
{{- if .Values.metrics.enabled }}
- name: metrics
  containerPort: {{ .Values.metrics.port }}
{{- end }}
{{- end -}}

{{/*
llama.cpp embedder args — a plain list driven entirely from
.Values.embedding.sidecar.args. No conditionals: edit values.yaml.
*/}}
{{- define "xray.embedderArgs" -}}
{{- toYaml .Values.embedding.sidecar.args }}
{{- end -}}
