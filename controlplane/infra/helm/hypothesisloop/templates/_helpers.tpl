{{- define "hypothesisloop.namespace" -}}
{{ .Values.namespace.name }}
{{- end -}}

{{/* call as: include "hypothesisloop.image" (dict "name" "control-service" "root" $) */}}
{{- define "hypothesisloop.image" -}}
{{ required "images.registry is required: pass the REGISTRY `make images` pushed to" .root.Values.images.registry }}/hypothesisloop-{{ .name }}:{{ required "images.tag is required: pass the TAG `make images` built" .root.Values.images.tag }}
{{- end -}}

{{/* env entries reading keys of the shared Secret. call as: include "hypothesisloop.secretEnv" (dict "keys" (list "A" "B") "root" $) */}}
{{- define "hypothesisloop.secretEnv" -}}
{{- range .keys }}
- name: {{ . }}
  valueFrom:
    secretKeyRef:
      name: {{ $.root.Values.secretName }}
      key: {{ . }}
{{- end }}
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
Replaces the value of one key of the staged config, found by its exact indentation and name.
Fails when the key is absent, so a renamed setting cannot leave the old value in place.
call as: include "hypothesisloop.setConfig" (dict "text" $raw "key" "  endpoint" "value" "http://x")
*/}}
{{- define "hypothesisloop.setConfig" -}}
{{- $pattern := printf "(?m)^%s:.*$" .key -}}
{{- if not (regexMatch $pattern .text) -}}
{{- fail (printf "files/hypothesisloop.yaml has no %q key" .key) -}}
{{- end -}}
{{ regexReplaceAllLiteral $pattern .text (printf "%s: %s" .key .value) }}
{{- end -}}

{{/* The bucket named by data_store.bucket in the staged config. */}}
{{- define "hypothesisloop.bucket" -}}
{{- $line := regexFind "(?m)^  bucket: \\S+" (include "hypothesisloop.stagedConfig" .) -}}
{{- if not $line -}}
{{- fail "files/hypothesisloop.yaml has no data_store.bucket" -}}
{{- end -}}
{{ trimPrefix "  bucket: " $line }}
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
