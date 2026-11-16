{{/* Labels every object carries. */}}
{{- define "canyon.labels" -}}
app.kubernetes.io/part-of: canyon
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{/* repo@sha256:digest from {repository, digest}; refuses an image without a digest. */}}
{{- define "canyon.image" -}}
{{- $img := index .root.Values.images .name -}}
{{- if not (and $img.repository $img.digest) -}}
{{- fail (printf "images.%s needs repository and digest (every image is pinned by digest)" .name) -}}
{{- end -}}
{{- if not (hasPrefix "sha256:" $img.digest) -}}
{{- fail (printf "images.%s.digest must be sha256:<hex>" .name) -}}
{{- end -}}
{{- printf "%s@%s" $img.repository $img.digest -}}
{{- end }}

{{/* A secret value: the one from values, else the key in the live Secret (so upgrades keep it), else
     generated. Memoised per ns/name/key, so every template that uses it gets the same value.
     Args: dict "root" $ "ns" "..." "name" "..." "key" "..." "value" <from values> */}}
{{- define "canyon.secret" -}}
{{- $cache := .root.Values._secretCache | default dict -}}
{{- $_ := set .root.Values "_secretCache" $cache -}}
{{- $id := printf "%s/%s/%s" .ns .name .key -}}
{{- if not (hasKey $cache $id) -}}
{{- $v := .value -}}
{{- if not $v -}}
{{- $live := lookup "v1" "Secret" .ns .name -}}
{{- if and $live $live.data (hasKey $live.data .key) -}}
{{- $v = index $live.data .key | b64dec -}}
{{- else -}}
{{- $v = randAlphaNum 40 -}}
{{- end -}}
{{- end -}}
{{- $_ := set $cache $id $v -}}
{{- end -}}
{{- index $cache $id -}}
{{- end }}

{{- define "canyon.domain" -}}
{{- required "global.domain is required" .Values.global.domain -}}
{{- end }}

{{- define "canyon.publicURL" -}}
{{- .Values.canyon.publicURL | default (printf "https://%s" (include "canyon.domain" .)) -}}
{{- end }}

{{- define "canyon.publicHost" -}}
{{- include "canyon.publicURL" . | trimPrefix "https://" | trimPrefix "http://" | splitList "/" | first | splitList ":" | first -}}
{{- end }}

{{- define "canyon.pocketIdURL" -}}
{{- printf "https://%s.%s" .Values.auth.pocketId.host (include "canyon.domain" .) -}}
{{- end }}

{{/* The executor's registry host. */}}
{{- define "canyon.registry" -}}
{{- if .Values.canyon.executor.registry -}}{{ .Values.canyon.executor.registry }}
{{- else if .Values.infra.registry.enabled -}}{{ printf "%s:5000" .Values.infra.registry.clusterIP }}
{{- else -}}{{ fail "canyon.executor.registry is required without infra.registry" }}
{{- end -}}
{{- end }}

{{/* Secret values shared across templates. */}}
{{- define "canyon.canyonPostgresPassword" -}}
{{- if .Values.infra.canyonPostgres.existingSecret -}}
{{- $s := lookup "v1" "Secret" "canyon-infra" .Values.infra.canyonPostgres.existingSecret -}}
{{- if $s -}}{{ index $s.data "password" | b64dec }}{{- else -}}{{ fail "infra.canyonPostgres.existingSecret not found in canyon-infra" }}{{- end -}}
{{- else -}}
{{- include "canyon.secret" (dict "root" . "ns" "canyon-infra" "name" "canyon-postgres" "key" "password" "value" .Values.infra.canyonPostgres.password) -}}
{{- end -}}
{{- end }}

{{- define "canyon.appPostgresPassword" -}}
{{- if .Values.infra.appPostgres.existingSecret -}}
{{- $s := lookup "v1" "Secret" "canyon-infra" .Values.infra.appPostgres.existingSecret -}}
{{- if $s -}}{{ index $s.data "password" | b64dec }}{{- else -}}{{ fail "infra.appPostgres.existingSecret not found in canyon-infra" }}{{- end -}}
{{- else -}}
{{- include "canyon.secret" (dict "root" . "ns" "canyon-infra" "name" "app-postgres" "key" "password" "value" .Values.infra.appPostgres.password) -}}
{{- end -}}
{{- end }}

{{- define "canyon.clientSecret" -}}
{{- $c := index .root.Values.auth.pocketId.clients .client -}}
{{- include "canyon.secret" (dict "root" .root "ns" "canyon-auth" "name" "pocket-id-clients" "key" .client "value" $c.secret) -}}
{{- end }}

{{/* The egress gateway's token (Secret canyon/canyon-egress): canyon.egress.token, else generated once and kept by
     lookup; "" with canyon.egress.existingSecret, which brings the Secret instead. */}}
{{- define "canyon.egressToken" -}}
{{- $e := .Values.canyon.egress -}}
{{- if $e.existingSecret -}}
{{- if $e.token -}}{{ fail "canyon.egress.token and canyon.egress.existingSecret: set one" }}{{- end -}}
{{- else -}}
{{- if and $e.token (not (regexMatch "^[A-Za-z0-9._~+/=-]{32,512}$" $e.token)) -}}
{{- fail "canyon.egress.token must be 32 to 512 of [A-Za-z0-9._~+/=-], the server's rule" -}}
{{- end -}}
{{- include "canyon.secret" (dict "root" . "ns" "canyon" "name" "canyon-egress" "key" "token" "value" $e.token) -}}
{{- end -}}
{{- end }}

{{/* A token from values puts its sum on the pods that read it, so a new one rolls Envoy and the server. A generated
     one never changes and carries none: under helm template it would be new at each render and roll both at every
     GitOps sync. A brought Secret carries none either. */}}
{{- define "canyon.egressTokenSum" -}}
{{- if .Values.canyon.egress.token }}checksum/egress-token: {{ include "canyon.egressToken" . | sha256sum | trunc 16 | quote }}{{ end -}}
{{- end }}

{{/* The token the bundled gateway asks /tls/ask with (#2672): Caddy is a pod with no address the server could
     trust on every network, so it proves itself with this instead. Generated once and kept by lookup in
     canyon/canyon-tls-ask; the gateway's own copy, canyon-gateway/caddy-tls-ask, is written from it. */}}
{{- define "canyon.tlsAskToken" -}}
{{- $t := .Values.gateway.askToken -}}
{{- if and $t (not (regexMatch "^[A-Za-z0-9._~-]{32,512}$" $t)) -}}
{{- fail "gateway.askToken must be 32 to 512 of [A-Za-z0-9._~-], the server's rule: it rides unencoded in Caddy's ask URL" -}}
{{- end -}}
{{- include "canyon.secret" (dict "root" . "ns" "canyon" "name" "canyon-tls-ask" "key" "token" "value" $t) -}}
{{- end }}

{{/* A token from values (gateway.askToken) puts its sum on Caddy's and the server's pods, so a new one rolls both
     together; a generated one never changes and carries none, as the egress token's (canyon.egressTokenSum). */}}
{{- define "canyon.tlsAskTokenSum" -}}
{{- if and .Values.gateway.enabled .Values.gateway.askToken }}checksum/tls-ask-token: {{ include "canyon.tlsAskToken" . | sha256sum | trunc 16 | quote }}{{ end -}}
{{- end }}

{{/* Sign-in settings, bundled or brought. */}}
{{- define "canyon.oidcIssuer" -}}
{{- if .Values.canyon.oidc.issuer -}}{{ .Values.canyon.oidc.issuer }}
{{- else if .Values.auth.pocketId.enabled -}}{{ include "canyon.pocketIdURL" . }}
{{- end -}}
{{- end }}
{{- define "canyon.oidcClientId" -}}
{{- if .Values.canyon.oidc.issuer -}}{{ .Values.canyon.oidc.clientId }}{{- else -}}{{ .Values.auth.pocketId.clients.canyon.id }}{{- end -}}
{{- end }}
{{- define "canyon.oidcClientSecret" -}}
{{- if .Values.canyon.oidc.issuer -}}{{ .Values.canyon.oidc.clientSecret }}{{- else if .Values.auth.pocketId.enabled -}}{{ include "canyon.clientSecret" (dict "root" . "client" "canyon") }}{{- end -}}
{{- end }}

{{- define "canyon.databaseURL" -}}
{{- if .Values.canyon.database.url -}}{{ .Values.canyon.database.url }}
{{- else if .Values.infra.canyonPostgres.enabled -}}{{ printf "postgres://canyon:%s@canyon-postgres.canyon-infra.svc.cluster.local:5432/canyon?sslmode=disable" (include "canyon.canyonPostgresPassword" .) }}
{{- else -}}{{ fail "canyon.database.url is required without infra.canyonPostgres" }}
{{- end -}}
{{- end }}

{{- define "canyon.appDatabaseURL" -}}
{{- if .Values.canyon.appDatabase.url -}}{{ .Values.canyon.appDatabase.url }}
{{- else if .Values.infra.appPostgres.enabled -}}{{ printf "postgres://canyon_admin:%s@app-postgres.canyon-infra.svc.cluster.local:5433/postgres?sslmode=disable" (include "canyon.appPostgresPassword" .) }}
{{- end -}}
{{- end }}

{{/* DNS egress, used by every policy. */}}
{{- define "canyon.dnsEgress" -}}
- to: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: kube-system}}, podSelector: {matchLabels: {k8s-app: kube-dns}}}]
  ports: [{protocol: UDP, port: 53}, {protocol: TCP, port: 53}]
{{- end }}

{{- define "canyon.private" -}}
[10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10, 169.254.0.0/16]
{{- end }}

{{/* What an app's egress, the egress gateway's and every build's leave out: executor/kube/objects.go's private. */}}
{{- define "canyon.floor" -}}
[10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10, 169.254.0.0/16, 0.0.0.0/8, 192.0.0.0/24, 198.18.0.0/15, 224.0.0.0/4, 240.0.0.0/4, 168.63.129.16/32]
{{- end }}

{{/* PVC with the keep policy: these hold data an uninstall must not delete. */}}
{{- define "canyon.pvc" -}}
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: {{ .name }}
  namespace: {{ .ns }}
  labels: {{- include "canyon.labels" .root | nindent 4 }}
  annotations: {helm.sh/resource-policy: keep}
spec:
  accessModes: [ReadWriteOnce]
  {{- with .root.Values.global.storageClass }}
  storageClassName: {{ . }}
  {{- end }}
  resources: {requests: {storage: {{ .size }}}}
{{- end }}

{{- /* canyon.ownedOrExit is the initContainer that lets a bundled part start only once its user can write .dir, its
     volume `data`, else exits at once with a line naming the fix (#2772): a volume a release before wrote as root —
     before this one, or rolled back to — would let the part start half-broken, the registry refusing pushes and Caddy
     storing no certificate. Every entry below .dir is the user's, or its group's (fsGroup, where the volume's driver
     applies it) with the group's write bits, and .dir takes a file. An initContainer, never a wrapper of the part's
     own start: an exec through sh loses Caddy's NET_BIND_SERVICE under no_new_privs. Same image, same user. */}}
{{- define "canyon.ownedOrExit" -}}
- name: volume-owned
  image: {{ include "canyon.image" (dict "root" .root "name" .image) }}
  command: [sh, -c, {{ printf `d=%s; u=$(id -u); g=$(id -g); bad=$( { find "$d" -mindepth 1 -type d ! -user "$u" ! \( -group "$g" -perm -070 \); find "$d" -mindepth 1 ! -type d ! -user "$u" ! \( -group "$g" -perm -060 \); } | head -n 3 | tr '\n' ' '); if [ -n "$bad" ] || ! touch "$d/.canyon-writable" || ! rm "$d/.canyon-writable"; then echo "canyon: user $u can't write $d ($(echo ${bad:-$d})). Hand it to user $u as docs/self-host/backup-and-upgrade.md says (The upgrade that runs the chart's bundled parts as no root), then delete this pod." >&2; exit 1; fi` .dir | quote }}]
  securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
  resources: {requests: {cpu: 10m, memory: 16Mi}, limits: {cpu: 200m, memory: 64Mi}}
  volumeMounts: [{name: data, mountPath: {{ .dir }}}]
{{- end }}
