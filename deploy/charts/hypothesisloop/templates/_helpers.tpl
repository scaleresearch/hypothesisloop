{{- define "hypothesisloop.namespace" -}}
{{ .Values.namespace.name }}
{{- end -}}

{{/* call as: include "hypothesisloop.image" (dict "name" "control-service" "root" $) */}}
{{- define "hypothesisloop.image" -}}
{{ required "images.registry is required: pass the REGISTRY `make publish` pushed to" .root.Values.images.registry }}/hypothesisloop-{{ .name }}:{{ .root.Values.images.tag | default .root.Chart.AppVersion }}
{{- end -}}

{{- define "hypothesisloop.secretName" -}}
{{ required "secretName is required: the Secret that holds every secret of the deployment. The chart reads it and never creates it" .Values.secretName }}
{{- end -}}

{{/* One env entry reading a key of the Secret. call as: include "hypothesisloop.secretKey" (dict "env" "DATABASE_URL" "key" "DATABASE_URL" "root" $) */}}
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
The keys each process reads, as controlplane/config names them (Settings.Needs): the stores' addresses
from the values and every secret from the Secret, under the same name.
*/}}
{{- define "hypothesisloop.metricsServiceEnv" -}}
- name: METRICS_DB_URL
  value: {{ include "hypothesisloop.metricsDBURL" . | quote }}
{{ include "hypothesisloop.secretEnv" (dict "keys" (list "DATABASE_URL" "AUTH_SIGNING_SECRET") "root" .) }}
{{- end -}}

{{/*
A provider's secret is the Secret key <SECTION>_<NAME>_<FIELD>, as controlplane/config names it, NAME the
section's provider or a cloud compute entry's cluster_name: the field the object store's, git host's and
registry's providers each declare, and API_KEY, the account key, of the sandbox and each cloud compute entry.
*/}}
{{- define "hypothesisloop.controlServiceEnv" -}}
{{- $keys := list "DATABASE_URL" "AUTH_SIGNING_SECRET" "MODEL_ANTHROPIC_OAUTH_TOKEN" (include "hypothesisloop.objectStoreSecretKey" .) -}}
{{- $gitHost := .Values.gitHost -}}
{{- if eq $gitHost.provider "github" -}}
{{- $keys = concat $keys (list "GIT_HOST_GITHUB_APP_PRIVATE_KEY" "GIT_HOST_GITHUB_CLIENT_SECRET") -}}
{{- else if and (eq $gitHost.provider "plain") $gitHost.plain.username -}}
{{- $keys = append $keys "GIT_HOST_PLAIN_PASSWORD" -}}
{{- end -}}
{{- with .Values.sandbox -}}
{{- $keys = append $keys (printf "SANDBOX_%s_API_KEY" (upper .provider)) -}}
{{- end -}}
{{- range .Values.cloudCompute -}}
{{- $keys = append $keys (printf "CLOUD_COMPUTE_%s_API_KEY" (upper (replace "-" "_" .cluster_name))) -}}
{{- end -}}
- name: METRICS_DB_URL
  value: {{ include "hypothesisloop.metricsDBURL" . | quote }}
- name: GRAPH_DB_URL
  value: {{ include "hypothesisloop.graphDBURL" . | quote }}
{{ include "hypothesisloop.secretEnv" (dict "keys" (uniq $keys) "root" .) }}
{{- end -}}

{{/* The Secret key of the registry's credential file, with containerRegistry.provider gcp. */}}
{{- define "hypothesisloop.containerRegistryCredentials" -}}
CONTAINER_REGISTRY_{{ upper .Values.containerRegistry.provider }}_CREDENTIALS
{{- end -}}

{{/* The container securityContext of every image built from this repository. */}}
{{- define "hypothesisloop.securityContext" -}}
securityContext:
  runAsNonRoot: true
  runAsUser: 65532
  runAsGroup: 65532
  readOnlyRootFilesystem: true
  allowPrivilegeEscalation: false
  capabilities:
    drop: ["ALL"]
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

{{/* The object_store section of the staged config, as YAML. call as: include "hypothesisloop.objectStore" . | fromYaml */}}
{{- define "hypothesisloop.objectStore" -}}
{{- $store := (include "hypothesisloop.stagedConfig" . | fromYaml).object_store -}}
{{- if not $store -}}
{{- fail "files/hypothesisloop.yaml has no object_store section" -}}
{{- end -}}
{{ toYaml $store }}
{{- end -}}

{{/* The Secret key of the object store's provider, which the staged config names. */}}
{{- define "hypothesisloop.objectStoreSecretKey" -}}
{{- $provider := (include "hypothesisloop.objectStore" . | fromYaml).provider -}}
{{- $field := get (dict "minio" "SECRET_ACCESS_KEY") (toString $provider) -}}
{{- if not $field -}}
{{- fail (printf "files/hypothesisloop.yaml names object_store provider %q, which the chart does not know" $provider) -}}
{{- end -}}
OBJECT_STORE_{{ upper $provider }}_{{ $field }}
{{- end -}}

{{- define "hypothesisloop.metricsDBURL" -}}
{{- if .Values.greptimedb.external.enabled -}}
{{ required "greptimedb.external.enabled is true but greptimedb.external.url is not set" .Values.greptimedb.external.url }}
{{- else -}}
http://greptimedb:4000
{{- end -}}
{{- end -}}

{{/* Where the control plane itself reaches the object store: the chart's own Service, or the store run elsewhere. */}}
{{- define "hypothesisloop.objectStoreControlPlaneEndpoint" -}}
{{- if .Values.objectStore.enabled -}}
{{- if .Values.objectStore.controlPlaneEndpoint -}}
{{- fail "objectStore.controlPlaneEndpoint is set while objectStore.enabled is true: the control plane reaches the store the chart runs at its Service" -}}
{{- end -}}
http://object-store:9000
{{- else -}}
{{ required "objectStore.controlPlaneEndpoint is required while objectStore.enabled is false: the address the control plane itself reaches the store at" .Values.objectStore.controlPlaneEndpoint }}
{{- end -}}
{{- end -}}

{{- define "hypothesisloop.graphDBURL" -}}
{{- if .Values.memgraph.external.enabled -}}
{{ required "memgraph.external.enabled is true but memgraph.external.url is not set" .Values.memgraph.external.url }}
{{- else -}}
bolt://memgraph:7687
{{- end -}}
{{- end -}}
