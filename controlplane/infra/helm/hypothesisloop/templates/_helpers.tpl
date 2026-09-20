{{- define "hypothesisloop.namespace" -}}
{{ .Values.namespace.name }}
{{- end -}}

{{/* call as: include "hypothesisloop.image" (dict "name" "control-service" "root" $) */}}
{{- define "hypothesisloop.image" -}}
{{ required "images.registry is required: pass the REGISTRY `make images` pushed to" .root.Values.images.registry }}/hypothesisloop-{{ .name }}:{{ required "images.tag is required: pass the TAG `make images` built" .root.Values.images.tag }}
{{- end -}}

{{- define "hypothesisloop.secretName" -}}
{{ required "secretName is required: the Secret that holds every secret of the deployment. The chart reads it and never creates it" .Values.secretName }}
{{- end -}}

{{/* One env entry reading a key of the Secret. call as: include "hypothesisloop.secretKey" (dict "env" "RUNTIME_TOKEN" "key" "CLOUD_AGENT_RUNTIME_TOKEN" "root" $) */}}
{{- define "hypothesisloop.secretKey" -}}
- name: {{ .env }}
  valueFrom:
    secretKeyRef:
      name: {{ include "hypothesisloop.secretName" .root }}
      key: {{ .key }}
{{- end -}}

{{/* Env entries named after the Secret keys they read. call as: include "hypothesisloop.secretEnv" (dict "keys" (list "A" "B") "root" $) */}}
{{- define "hypothesisloop.secretEnv" -}}
{{- range .keys }}
{{ include "hypothesisloop.secretKey" (dict "env" . "key" . "root" $.root) }}
{{- end }}
{{- end -}}

{{/*
Every ${NAME} of the rendered hypothesisloop.yaml, for the two processes that load it. DATABASE_URL
is in no file: both read it from the environment.
*/}}
{{- define "hypothesisloop.configSecretEnv" -}}
{{- $keys := list "DATABASE_URL" "HYPOTHESISLOOP_SIGNING_SECRET" "OBJECT_STORE_ACCESS_KEY_ID" "OBJECT_STORE_SECRET_ACCESS_KEY" -}}
{{- if .Values.github.appId -}}
{{- $keys = concat $keys (list "HYPOTHESISLOOP_GITHUB_APP_CLIENT_SECRET" "HYPOTHESISLOOP_GITHUB_APP_PRIVATE_KEY") -}}
{{- end -}}
{{ include "hypothesisloop.secretEnv" (dict "keys" $keys "root" .) }}
{{- end -}}

{{/*
The init container of a runtime process: the control plane's own tool, started with what
control-service is started with, leaves the process's token in /token/token. The token is derived
from the signing secret, so no one has to carry it into the Secret.
call as: include "hypothesisloop.tokenInit" (dict "script" "/hl-admin token launcher:main > /token/token" "root" $)
*/}}
{{- define "hypothesisloop.tokenInit" -}}
- name: token
  image: {{ include "hypothesisloop.image" (dict "name" "control-service" "root" .root) }}
  imagePullPolicy: {{ .root.Values.images.pullPolicy }}
  command: ["/bin/sh", "-ec", {{ .script | quote }}]
  env:
    - name: HYPOTHESISLOOP_CONFIG
      value: /platform/hypothesisloop.yaml
    {{- include "hypothesisloop.configSecretEnv" .root | nindent 4 }}
  volumeMounts:
    - name: token
      mountPath: /token
    - name: platform-config
      mountPath: /platform
      readOnly: true
{{- end -}}

{{/* The two volumes hypothesisloop.tokenInit mounts. */}}
{{- define "hypothesisloop.tokenVolumes" -}}
- name: token
  emptyDir:
    medium: Memory
- name: platform-config
  configMap:
    name: hypothesisloop-config
{{- end -}}

{{/* Pod annotations that restart a pod when the Secret it reads changes. */}}
{{- define "hypothesisloop.secretAnnotations" -}}
checksum/secret: {{ .Values.secretChecksum | quote }}
{{- end -}}

{{/* The staged hypothesisloop.yaml. */}}
{{- define "hypothesisloop.stagedConfig" -}}
{{- $raw := .Files.Get "files/hypothesisloop.yaml" -}}
{{- if not $raw -}}
{{- fail "files/hypothesisloop.yaml is missing: run `make helm-prepare` first" -}}
{{- end -}}
{{ $raw }}
{{- end -}}

{{/*
A top-level section of the staged config from its name down to the line of one of its own keys.
Fails when the section has no such key, so a renamed setting cannot go unnoticed.
call as: include "hypothesisloop.configKey" (dict "text" $raw "section" "auth" "key" "issuer")
*/}}
{{- define "hypothesisloop.configKey" -}}
{{- $found := regexFind (printf "(?m)^%s:[ \\t]*\\n(?:[ #].*\\n|\\n)*?  %s:.*$" .section .key) .text -}}
{{- if not $found -}}
{{- fail (printf "files/hypothesisloop.yaml has no %s.%s" .section .key) -}}
{{- end -}}
{{ $found }}
{{- end -}}

{{/*
Replaces the value of one key of one section of the staged config. The value is written as JSON,
which YAML reads as it is, so no value can change the shape of the file.
call as: include "hypothesisloop.setConfig" (dict "text" $raw "section" "auth" "key" "issuer" "value" "https://issuer")
*/}}
{{- define "hypothesisloop.setConfig" -}}
{{- $found := include "hypothesisloop.configKey" . -}}
{{- $line := printf "  %s: %s" .key (toJson .value) -}}
{{ replace $found (print (regexReplaceAllLiteral "(?m)^.*\\z" $found "") $line) .text }}
{{- end -}}

{{/* The bucket named by data_store.bucket in the staged config. */}}
{{- define "hypothesisloop.bucket" -}}
{{- $found := include "hypothesisloop.configKey" (dict "text" (include "hypothesisloop.stagedConfig" .) "section" "data_store" "key" "bucket") -}}
{{- $bucket := regexReplaceAll "^  bucket:[ \\t]*(\\S*).*$" (regexFind "[^\\n]*\\z" $found) "${1}" | trimAll "\"'" -}}
{{- if not $bucket -}}
{{- fail "files/hypothesisloop.yaml has an empty data_store.bucket" -}}
{{- end -}}
{{ $bucket }}
{{- end -}}

{{- define "hypothesisloop.metricsDBURL" -}}
{{- if .Values.greptimedb.external.enabled -}}
{{ required "greptimedb.external.enabled is true but greptimedb.external.url is not set" .Values.greptimedb.external.url }}
{{- else -}}
http://greptimedb:4000
{{- end -}}
{{- end -}}

{{- define "hypothesisloop.graphDBURL" -}}
{{- if .Values.memgraph.external.enabled -}}
{{ required "memgraph.external.enabled is true but memgraph.external.url is not set" .Values.memgraph.external.url }}
{{- else -}}
bolt://memgraph:7687
{{- end -}}
{{- end -}}
