{{/* Standard name helpers. */}}
{{- define "xray-graph.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "xray-graph.fullname" -}}
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

{{- define "xray-graph.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "xray-graph.labels" -}}
helm.sh/chart: {{ include "xray-graph.chart" . }}
{{ include "xray-graph.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "xray-graph.selectorLabels" -}}
app.kubernetes.io/name: {{ include "xray-graph.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "xray-graph.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "xray-graph.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
The claim the snapshot volume binds to: an existing one when named, otherwise
the one this chart creates.
*/}}
{{- define "xray-graph.pvcName" -}}
{{- if .Values.snapshots.pvc.existingClaim }}
{{- .Values.snapshots.pvc.existingClaim }}
{{- else }}
{{- printf "%s-maps" (include "xray-graph.fullname" .) }}
{{- end }}
{{- end }}

{{/*
serve-snapshots arguments. Built in one place so the deployment template stays
a list of volumes and probes, and so a flag is spelled once.
*/}}
{{- define "xray-graph.args" -}}
- graph
- serve-snapshots
- -snapshot-dir={{ .Values.snapshots.mountPath }}
- -addr=:{{ .Values.server.port }}
{{- if .Values.embeddings.sidecar.enabled }}
- -embedding-endpoint=http://localhost:{{ .Values.embeddings.sidecar.port }}
{{- else }}
- -embedding-endpoint={{ required "embeddings.endpoint is required" .Values.embeddings.endpoint }}
{{- end }}
- -embedding-model={{ .Values.embeddings.model }}
- -embedding-dims={{ .Values.embeddings.dims }}
- -embedding-max-input-chars={{ .Values.embeddings.maxInputChars }}
- -graph-ttl-min={{ .Values.server.graphTTLMinutes }}
- -route-fanout={{ .Values.server.routeFanout }}
- -skills-map={{ .Values.server.skillsMap }}
- -memory-map={{ .Values.server.memoryMap }}
- -log-level={{ .Values.server.logLevel }}
{{- if .Values.server.writable }}
- -writable
{{- end }}
{{- with .Values.extraArgs }}
{{- toYaml . | nindent 0 }}
{{- end }}
{{- end }}

{{/*
llama.cpp sidecar arguments: the baked model at modelPath (--model), then the
tuning flags. Built here so the deployment template stays a list of probes and
mounts, and so a flag is spelled once.
*/}}
{{- define "xray-graph.embedderArgs" -}}
{{- $s := .Values.embeddings.sidecar -}}
- --host
- 0.0.0.0
- --port
- {{ $s.port | quote }}
- --embeddings
- --model
- {{ $s.modelPath | quote }}
{{- with $s.extraArgs }}
{{- toYaml . | nindent 0 }}
{{- end }}
{{- end }}
