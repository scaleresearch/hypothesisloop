{{- define "hypothesisloop.namespace" -}}
{{ .Values.namespace.name }}
{{- end -}}

{{/* call as: include "hypothesisloop.image" (dict "name" "control-service" "root" $) */}}
{{- define "hypothesisloop.image" -}}
{{ required "images.registry is required -- run `make helm-push` and pass its REGISTRY, or point at your own registry" .root.Values.images.registry }}/hypothesisloop-{{ .name }}:{{ required "images.tag is required -- run `make helm-push` and pass the TAG it built" .root.Values.images.tag }}
{{- end -}}

{{/*
Postgres connection string, used only to fill the hypothesisloop-db Secret this chart creates
itself -- skipped entirely when postgres.external.existingSecret names a Secret the operator
already created (see control-service.yaml/metrics-service.yaml's DATABASE_URL env, which then
reads that Secret directly instead of this one). In-cluster DNS by default; a managed instance
(postgres.external.enabled + url) supplies its own connection string instead.
*/}}
{{- define "hypothesisloop.databaseURL" -}}
{{- if .Values.postgres.external.enabled -}}
{{- if not .Values.postgres.external.url -}}
{{- fail "postgres.external.enabled is true but neither postgres.external.url nor postgres.external.existingSecret is set" -}}
{{- end -}}
{{ .Values.postgres.external.url }}
{{- else -}}
postgres://{{ .Values.postgres.user }}:{{ .Values.postgres.password }}@hypothesisloop-postgres:5432/{{ .Values.postgres.database }}?sslmode=disable
{{- end -}}
{{- end -}}

{{/* Name of the Secret holding DATABASE_URL: the operator's own when set, else ours. */}}
{{- define "hypothesisloop.databaseSecretName" -}}
{{- if .Values.postgres.external.existingSecret -}}
{{ .Values.postgres.external.existingSecret }}
{{- else -}}
hypothesisloop-db
{{- end -}}
{{- end -}}

{{/* Key within that Secret holding DATABASE_URL. */}}
{{- define "hypothesisloop.databaseSecretKey" -}}
{{- if .Values.postgres.external.existingSecret -}}
{{ .Values.postgres.external.existingSecretKey }}
{{- else -}}
DATABASE_URL
{{- end -}}
{{- end -}}

{{/*
GreptimeDB metrics_db_url, rendered into the staged hypothesisloop.yaml ConfigMap
(templates/config.yaml). Cannot share hypothesisloop.databaseURL above -- GreptimeDB's URL is
a bare http:// host:port, not a postgres:// connection string -- so it gets its own helper.
existingSecret is deliberately not read here: a ConfigMap is plaintext, so pulling a Secret's
value into it at template time would defeat the point of a Secret. Use greptimedb.external.url
for a managed instance (its address is not normally sensitive the way a DB password is); route
real credentials to GreptimeDB itself via whatever auth its own client config supports, not
through this file.
*/}}
{{- define "hypothesisloop.metricsDBURL" -}}
{{- if .Values.greptimedb.external.enabled -}}
{{- if not .Values.greptimedb.external.url -}}
{{- fail "greptimedb.external.enabled is true but greptimedb.external.url is not set (existingSecret is not supported here -- see this helper's comment)" -}}
{{- end -}}
{{ .Values.greptimedb.external.url }}
{{- else -}}
http://greptimedb:4000
{{- end -}}
{{- end -}}
