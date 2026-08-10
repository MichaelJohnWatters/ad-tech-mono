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

{{/*
Fully-qualified image ref for an app-service image. Local dev leaves
global.image.registry empty, so the image stays the bare locally-built name
(adtech-<svc>, resolved from the node's docker engine). Staging/prod set
global.image.registry (e.g. ghcr.io/<owner>) + global.image.tag (a git sha or
moving tag from CI) and every APP image becomes <registry>/<name>:<tag>. Infra
images (postgres, nats, clickhouse, minio, redis) are third-party and bypass
this helper — they carry their own pinned refs.
Call: include "adtech.image" (dict "name" $svc.image "g" $.Values.global)
*/}}
{{- define "adtech.image" -}}
{{- $g := .g | default dict -}}
{{- $img := ($g.image | default dict) -}}
{{- $registry := ($img.registry | default "") -}}
{{- $tag := ($img.tag | default "") -}}
{{- if $registry -}}
{{- printf "%s/%s" $registry .name -}}{{- if $tag -}}:{{ $tag }}{{- end -}}
{{- else -}}
{{- .name -}}
{{- end -}}
{{- end -}}
