{{- define "gitlab-doc-platform.name" -}}gitlab-doc-platform{{- end -}}
{{- define "gitlab-doc-platform.llmEndpoint" -}}
{{- if .Values.llm.endpoint -}}
{{- .Values.llm.endpoint -}}
{{- else if eq .Values.llm.provider "openai" -}}
https://api.openai.com/v1
{{- else if eq .Values.llm.provider "anthropic" -}}
https://api.anthropic.com/v1
{{- else -}}
http://gitlab-doc-platform-ollama:11434
{{- end -}}
{{- end -}}
{{- define "gitlab-doc-platform.llmModel" -}}
{{- if .Values.llm.model -}}
{{- .Values.llm.model -}}
{{- else if eq .Values.llm.provider "ollama" -}}
qwen2.5-coder:7b
{{- else -}}
{{- fail "llm.model is required for openai and anthropic" -}}
{{- end -}}
{{- end -}}
{{- define "gitlab-doc-platform.labels" -}}
app.kubernetes.io/name: {{ include "gitlab-doc-platform.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
{{- define "gitlab-doc-platform.storageAffinity" -}}
{{- if .Values.storage.coLocate }}
affinity:
  podAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/instance: {{ .Release.Name }}
          matchExpressions:
            - key: app.kubernetes.io/component
              operator: In
              values: ["api", "worker"]
        topologyKey: kubernetes.io/hostname
{{- end }}
{{- end -}}
