{{/*
Target namespace. Matches the kustomize base (namespace: adtech).
Deviation from helm norms: we deliberately do NOT stamp the usual
helm.sh/chart / app.kubernetes.io/* label boilerplate — the kustomize
manifests use plain `app: <name>` labels as pod selectors, and Services must
keep matching pods deployed by either toolchain during a migration.
*/}}
{{- define "adtech.namespace" -}}
{{- .Values.namespace | default "adtech" -}}
{{- end -}}

{{/*
Labels for a named app. `app: <name>` matches the CURRENT manifests'
selector labels exactly.
*/}}
{{- define "adtech.labels" -}}
app: {{ . }}
{{- end -}}
