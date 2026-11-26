{{- define "ward.image" -}}
{{- $image := required "image.repository is required" .Values.image.repository -}}
{{- if .Values.image.digest -}}
{{- if not (hasPrefix "sha256:" .Values.image.digest) }}{{ fail "image.digest must be a sha256 digest" }}{{ end -}}
{{- printf "%s@%s" $image .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" $image (required "image.digest or image.tag is required" .Values.image.tag) -}}
{{- end -}}
{{- end -}}

{{- define "ward.serviceAccountName" -}}
{{- default .Release.Name .Values.serviceAccount.name -}}
{{- end -}}

{{- define "ward.labels" -}}
app.kubernetes.io/name: ward
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end -}}

{{- define "ward.selector" -}}
app.kubernetes.io/name: ward
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* ward.mirrors renders WARD_MIRRORS. Each credential is a file that the Deployment mounts under /var/run/ward/mirrors/NAME. */}}
{{- define "ward.mirrors" -}}
{{- $out := list -}}
{{- range .Values.mirrors -}}
{{- $dir := printf "/var/run/ward/mirrors/%s" .name -}}
{{- $e := dict "name" .name "repo" .repo "remote" .remote "refs" .refs "prune_unknown" (default false .pruneUnknown) "seed" (default false .seed) -}}
{{- if and .credential .githubApp }}{{ fail (printf "mirror %s: set only one of credential and githubApp" .name) }}{{ end -}}
{{- if .credential -}}
{{- $_ := set $e "credential_file" (printf "%s/credential" $dir) -}}
{{- if .username }}{{ $_ := set $e "username" .username }}{{ end -}}
{{- else if .githubApp -}}
{{- $_ := set $e "github_app" (dict "app_id" (int64 .githubApp.appId) "installation_id" (int64 .githubApp.installationId) "key_file" (printf "%s/credential" $dir)) -}}
{{- else -}}
{{- fail (printf "mirror %s: set credential or githubApp" .name) -}}
{{- end -}}
{{- $out = append $out $e -}}
{{- end -}}
{{- toJson $out -}}
{{- end -}}

{{/* ward.runners renders WARD_RUNNERS. */}}
{{- define "ward.runners" -}}
{{- $out := list -}}
{{- range $r := .Values.runners -}}
{{- $e := dict "repo" (required "runner: repo is required" $r.repo) "kids" (required (printf "runner %s: kids is required" $r.repo) $r.kids) "env" (default "" $r.env) -}}
{{- if $r.minIsolation }}{{ $_ := set $e "min_isolation" $r.minIsolation }}{{ end -}}
{{- $out = append $out $e -}}
{{- end -}}
{{- toJson $out -}}
{{- end -}}

{{/* The runner selector differs from the ward selector, so the Service of ward never routes to a runner pod. */}}
{{- define "ward.runner.selector" -}}
app.kubernetes.io/name: ward-runner
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "ward.runner.labels" -}}
{{ include "ward.runner.selector" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end -}}
