{{/*
USSLP chart helpers.

Naming: every object is <release>-<service>, so two releases of the chart in one
namespace (a blue/green region cutover, say) never collide.
*/}}

{{- define "usslp.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "usslp.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "usslp.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels. app.kubernetes.io/* is the recommended set; the usslp.io/* labels
are what the Gatekeeper "required labels" constraint and the NetworkPolicies
select on, and what makes a query for "everything on the price path" possible.
*/}}
{{- define "usslp.labels" -}}
helm.sh/chart: {{ include "usslp.chart" . }}
{{ include "usslp.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: usslp
usslp.io/region: {{ .Values.global.region | quote }}
usslp.io/environment: {{ .Values.global.environment | quote }}
{{- end -}}

{{- define "usslp.selectorLabels" -}}
app.kubernetes.io/name: {{ include "usslp.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Per-service labels. Call as (dict "ctx" $ "name" $name "svc" $svc).
usslp.io/tier is derived from the priority class, so "which workloads are on the
price path" is answerable with a label selector rather than a wiki page.
*/}}
{{- define "usslp.serviceLabels" -}}
{{- $ctx := .ctx -}}
{{ include "usslp.labels" $ctx }}
app.kubernetes.io/component: {{ .name }}
usslp.io/service: {{ .name }}
usslp.io/tier: {{ trimPrefix "usslp-" (default "usslp-platform" .svc.priorityClassName) }}
{{- end -}}

{{- define "usslp.serviceSelectorLabels" -}}
{{- $ctx := .ctx -}}
{{ include "usslp.selectorLabels" $ctx }}
app.kubernetes.io/component: {{ .name }}
{{- end -}}

{{/*
Fully-qualified image reference.

Third-party workloads (EMQX, Kafka Connect, the Kafka CLI used by the topic job)
carry their own `registry` and `tag`; USSLP's own images take the registry from
global.imageRegistry and the tag from image.tag, defaulting to the chart's
appVersion. `:latest` is never produced here, and admission policy rejects it
anyway.
*/}}
{{- define "usslp.image" -}}
{{- $ctx := .ctx -}}
{{- $img := .svc.image -}}
{{- $registry := default $ctx.Values.global.imageRegistry $img.registry -}}
{{- $tag := default (default $ctx.Chart.AppVersion $ctx.Values.image.tag) $img.tag -}}
{{- printf "%s/%s:%s" $registry $img.repository $tag -}}
{{- end -}}

{{/*
Pod security context, with per-service overrides merged over the chart default.
EMQX and Kafka Connect run as uid 1000 in their upstream images; everything
USSLP builds runs as 65532.
*/}}
{{- define "usslp.podSecurityContext" -}}
{{- $base := deepCopy .ctx.Values.podSecurityContext -}}
{{- $merged := mergeOverwrite $base (default (dict) .svc.podSecurityContextOverrides) -}}
{{- toYaml $merged -}}
{{- end -}}

{{- define "usslp.containerSecurityContext" -}}
{{- $base := deepCopy .ctx.Values.containerSecurityContext -}}
{{- $merged := mergeOverwrite $base (default (dict) .svc.securityContextOverrides) -}}
{{- toYaml $merged -}}
{{- end -}}

{{/*
Topology spread.

Both constraints are ScheduleAnyway. DoNotSchedule on a zone constraint turns a
single-AZ outage into an inability to reschedule the pods that survived it,
which converts a partial outage into a total one — exactly the failure the
constraint exists to prevent.
*/}}
{{- define "usslp.topologySpread" -}}
{{- $ctx := .ctx -}}
{{- if $ctx.Values.topologySpread.enabled }}
- maxSkew: {{ $ctx.Values.topologySpread.zone.maxSkew }}
  topologyKey: {{ $ctx.Values.topologySpread.zone.topologyKey }}
  whenUnsatisfiable: {{ $ctx.Values.topologySpread.zone.whenUnsatisfiable }}
  labelSelector:
    matchLabels:
      {{- include "usslp.serviceSelectorLabels" (dict "ctx" $ctx "name" .name) | nindent 6 }}
- maxSkew: {{ $ctx.Values.topologySpread.node.maxSkew }}
  topologyKey: {{ $ctx.Values.topologySpread.node.topologyKey }}
  whenUnsatisfiable: {{ $ctx.Values.topologySpread.node.whenUnsatisfiable }}
  labelSelector:
    matchLabels:
      {{- include "usslp.serviceSelectorLabels" (dict "ctx" $ctx "name" .name) | nindent 6 }}
{{- end -}}
{{- end -}}

{{/*
Environment variables for one service: the declared map, plus the NAME_FILE
indirections its secrets contribute, plus the identity every binary reads.

config.Loader resolves NAME_FILE before NAME (platform/pkg/config/config.go), so
a projected secret file always wins over an environment variable of the same
name — which is why credentials are mounted rather than injected.
*/}}
{{- define "usslp.env" -}}
{{- $ctx := .ctx -}}
{{- $svc := .svc -}}
- name: USSLP_REGION
  value: {{ $ctx.Values.global.region | quote }}
- name: USSLP_ENVIRONMENT
  value: {{ $ctx.Values.global.environment | quote }}
- name: USSLP_VERSION
  value: {{ default $ctx.Chart.AppVersion $ctx.Values.image.tag | quote }}
- name: POD_NAME
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
- name: POD_NAMESPACE
  valueFrom:
    fieldRef:
      fieldPath: metadata.namespace
- name: NODE_NAME
  valueFrom:
    fieldRef:
      fieldPath: spec.nodeName
{{- range $k, $v := $svc.env }}
- name: {{ $k }}
  value: {{ $v | quote }}
{{- end }}
{{- range $secret := default (list) $svc.secrets }}
{{- range $k, $v := default (dict) $secret.env }}
- name: {{ $k }}
  value: {{ $v | quote }}
{{- end }}
{{- range $k, $v := default (dict) $secret.envFiles }}
- name: {{ $k }}
  value: {{ $v | quote }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
Volumes: the state directory plus one projected volume per declared secret.
*/}}
{{- define "usslp.volumes" -}}
{{- $ctx := .ctx -}}
{{- $svc := .svc -}}
{{- $name := .name -}}
{{- if and $svc.storage (eq (default "emptyDir" $svc.storage.kind) "emptyDir") }}
- name: state
  emptyDir:
    sizeLimit: {{ $svc.storage.sizeLimit }}
{{- end }}
{{- range $secret := default (list) $svc.secrets }}
- name: secret-{{ $secret.name }}
  secret:
    secretName: {{ include "usslp.fullname" $ctx }}-{{ $name }}-{{ $secret.name }}
    defaultMode: 0400
{{- end }}
{{- end -}}

{{- define "usslp.volumeMounts" -}}
{{- $svc := .svc -}}
{{- if and $svc.storage (eq (default "emptyDir" $svc.storage.kind) "emptyDir") }}
- name: state
  mountPath: {{ $svc.storage.mountPath }}
{{- end }}
{{- if and $svc.storage (eq (default "emptyDir" $svc.storage.kind) "persistentVolumeClaim") }}
- name: state
  mountPath: {{ $svc.storage.mountPath }}
{{- end }}
{{- range $secret := default (list) $svc.secrets }}
{{- if $secret.mountPath }}
- name: secret-{{ $secret.name }}
  mountPath: {{ $secret.mountPath }}
  readOnly: true
{{- end }}
{{- end }}
{{- end -}}

{{/*
Probes.

Liveness is /healthz and asks only "is this process scheduling goroutines" —
platform/pkg/obs/admin.go answers it unconditionally with 200. Readiness is
/readyz and runs the registered dependency checks. That asymmetry is deliberate
and load-bearing: a broker blip must remove a pod from the endpoint list, never
restart it, or a five-second dependency wobble becomes a cluster-wide restart
storm (INTERFACE-CONTRACTS section 7).

failureThreshold on liveness is high for the same reason: the only thing that
should restart a USSLP pod is a genuinely wedged process.
*/}}
{{- define "usslp.probes" -}}
{{- $svc := .svc -}}
{{- if and $svc.probes $svc.probes.exec }}
livenessProbe:
  exec:
    command: ["/opt/emqx/bin/emqx", "ctl", "status"]
  initialDelaySeconds: 60
  periodSeconds: 30
  timeoutSeconds: 10
  failureThreshold: 6
readinessProbe:
  exec:
    command: ["/opt/emqx/bin/emqx", "ctl", "status"]
  initialDelaySeconds: 20
  periodSeconds: 10
  timeoutSeconds: 10
  failureThreshold: 3
startupProbe:
  exec:
    command: ["/opt/emqx/bin/emqx", "ctl", "status"]
  periodSeconds: 10
  failureThreshold: 30
{{- else if and $svc.probes $svc.probes.httpPath }}
livenessProbe:
  httpGet:
    path: {{ $svc.probes.httpPath }}
    port: admin
  initialDelaySeconds: 60
  periodSeconds: 30
  timeoutSeconds: 10
  failureThreshold: 6
readinessProbe:
  httpGet:
    path: {{ $svc.probes.readyPath }}
    port: admin
  initialDelaySeconds: 20
  periodSeconds: 10
  timeoutSeconds: 5
  failureThreshold: 3
startupProbe:
  httpGet:
    path: {{ $svc.probes.readyPath }}
    port: admin
  periodSeconds: 10
  failureThreshold: 60
{{- else }}
livenessProbe:
  httpGet:
    path: /healthz
    port: admin
  periodSeconds: 20
  timeoutSeconds: 3
  failureThreshold: 6
readinessProbe:
  httpGet:
    path: /readyz
    port: admin
  periodSeconds: 5
  timeoutSeconds: 3
  successThreshold: 1
  failureThreshold: 3
startupProbe:
  httpGet:
    path: /readyz
    port: admin
  periodSeconds: 3
  timeoutSeconds: 3
  failureThreshold: 60
{{- end }}
{{- end -}}

{{/*
The preStop hook.

obs.Runtime.Shutdown fails readiness first and then drains, but endpoint removal
is eventually consistent: kube-proxy and the mesh sidecar learn about it after
the process has already stopped accepting. Sleeping before SIGTERM gives them
time, which is the difference between a rolling update with zero 502s and one
with a handful per pod.

The images have no shell, so this cannot be a shell command. It is the
`sleep` action, which the kubelet performs itself (Kubernetes 1.29+; on 1.28
with the PodLifecycleSleepAction gate).
*/}}
{{- define "usslp.lifecycle" -}}
preStop:
  sleep:
    seconds: 5
{{- end -}}

{{/*
Validation. Fails the render rather than deploying a placeholder.
*/}}
{{- define "usslp.validate" -}}
{{- if and (ne .Values.global.environment "dev") (contains "REPLACE-ME" .Values.externalSecrets.remotePathPrefix) -}}
{{- fail "externalSecrets.remotePathPrefix still contains REPLACE-ME; set it in the environment values file" -}}
{{- end -}}
{{- if and .Values.ingress.enabled (contains "REPLACE-ME" .Values.ingress.host) -}}
{{- fail "ingress.enabled is true but ingress.host still contains REPLACE-ME" -}}
{{- end -}}
{{- if and .Values.topicProvisioning.enabled (empty .Values.topicProvisioning.bootstrapServers) -}}
{{- fail "topicProvisioning.enabled is true but topicProvisioning.bootstrapServers is empty" -}}
{{- end -}}
{{- range $name, $svc := .Values.services -}}
{{- if $svc.enabled -}}
{{- if lt (int $svc.replicas.min) (int (add (int $svc.pdb.minAvailable) 1)) -}}
{{/*
A PDB minAvailable equal to the replica floor makes every voluntary disruption
impossible: a node drain blocks forever and the cluster autoscaler stalls. The
floor must exceed the budget by at least one.
*/}}
{{- fail (printf "service %s: replicas.min (%v) must exceed pdb.minAvailable (%v) by at least 1" $name $svc.replicas.min $svc.pdb.minAvailable) -}}
{{- end -}}
{{- if le (int $svc.terminationGracePeriodSeconds) (int $svc.drainTimeoutSeconds) -}}
{{/*
Kubernetes sends SIGTERM and waits terminationGracePeriodSeconds before
SIGKILL. If that is not longer than the app's own drain, the drain is killed
half-finished — for the Label Service that means some shelves showing the new
promotion and some the old price.
*/}}
{{- fail (printf "service %s: terminationGracePeriodSeconds (%v) must exceed drainTimeoutSeconds (%v)" $name $svc.terminationGracePeriodSeconds $svc.drainTimeoutSeconds) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
