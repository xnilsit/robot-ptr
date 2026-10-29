{{/*
Prints "true" when more than one replica runs, which requires leader election.
*/}}
{{- define "robot-ptr.leaderElection" -}}
  {{- if gt (dig "controllers" "main" "replicas" 1 .Values | int) 1 -}}true{{- end -}}
{{- end -}}

{{/*
Prints "true" unless config.resetDelay is a zero duration ("0", "0s", "0m0s",
...). Only a reset delay makes the controller write the pod-left-at node
annotation.
*/}}
{{- define "robot-ptr.resetDelay" -}}
  {{- $delay := required "config.resetDelay is required" .Values.config.resetDelay | toString -}}
  {{- if regexReplaceAll "[+0.]|ns|us|µs|ms|s|m|h" $delay "" -}}true{{- end -}}
{{- end -}}

{{/*
RBAC subject of the controller's ServiceAccount.
*/}}
{{- define "robot-ptr.subject" -}}
  {{- $sa := dig "controllers" "main" "serviceAccount" dict .Values -}}
  {{- if $sa.identifier -}}
identifier: {{ $sa.identifier }}
  {{- else if $sa.name -}}
kind: ServiceAccount
name: {{ $sa.name }}
namespace: {{ .Release.Namespace }}
  {{- else -}}
    {{- $enabled := include "bjw-s.common.lib.serviceAccount.enabledServiceAccounts" (dict "rootContext" .) | fromYaml -}}
    {{- if ne (len $enabled) 1 -}}
      {{- fail "robot-ptr: enable exactly one serviceAccount or set controllers.main.serviceAccount" -}}
    {{- end -}}
identifier: {{ keys $enabled | first }}
  {{- end -}}
{{- end -}}

{{/*
Values merged over the user values before the common library renders them.
*/}}
{{- define "robot-ptr.values" -}}
  {{- $cfg := .Values.config -}}
  {{- $leaderElection := include "robot-ptr.leaderElection" . -}}

  {{- $args := list
    (printf "--annotation-prefix=%s" (required "config.annotationPrefix is required" $cfg.annotationPrefix))
    (printf "--default-ptr=%s" ($cfg.defaultPTR | default ""))
    (printf "--reset-delay=%s" ($cfg.resetDelay | toString))
    (printf "--resync-interval=%s" (required "config.resyncInterval is required" $cfg.resyncInterval | toString))
  -}}
  {{- with $cfg.nodeSelector -}}
    {{- $args = append $args (printf "--node-selector=%s" .) -}}
  {{- end -}}
  {{- with $cfg.robotURL -}}
    {{- $args = append $args (printf "--robot-url=%s" .) -}}
  {{- end -}}
  {{- with $cfg.hcloudEndpoint -}}
    {{- $args = append $args (printf "--hcloud-endpoint=%s" .) -}}
  {{- end -}}
  {{- with $cfg.logLevel -}}
    {{- $args = append $args (printf "--zap-log-level=%v" .) -}}
  {{- end -}}
  {{- if $cfg.hccmManaged -}}
    {{- $args = append $args "--hccm-managed" -}}
  {{- end -}}
  {{- if $cfg.dryRun -}}
    {{- $args = append $args "--dry-run" -}}
  {{- end -}}
  {{- if $leaderElection -}}
    {{- $args = append $args "--leader-elect" -}}
  {{- end -}}
  {{- $args = concat $args (dig "controllers" "main" "containers" "main" "args" list .Values) -}}

  {{- $subject := include "robot-ptr.subject" . | fromYaml -}}
  {{- $fullname := include "bjw-s.common.lib.chart.names.fullname" . -}}
global:
  templateEvaluation:
    # Never evaluate the credentials as templates.
    excludePaths: {{ append (dig "global" "templateEvaluation" "excludePaths" list .Values) (list "secrets" "credentials") | toJson }}
controllers:
  main:
    {{- /* Pods may only overlap during rollouts when leader election is on. */}}
    {{- if not (dig "controllers" "main" "strategy" "" .Values) }}
    strategy: {{ ternary "RollingUpdate" "Recreate" (eq $leaderElection "true") }}
    {{- end }}
    containers:
      main:
        args: {{ $args | toJson }}
        envFrom:
          - secretRef:
              {{- with .Values.credentials.existingSecret }}
              name: {{ . }}
              {{- else }}
              identifier: credentials
              {{- end }}
{{- with .Values.credentials }}
{{- if not .existingSecret }}
{{- if not (or .user .password .hcloudToken) }}
  {{- fail "set credentials.user and credentials.password, credentials.hcloudToken, or credentials.existingSecret" }}
{{- end }}
secrets:
  credentials:
    stringData:
      {{- if or .user .password }}
      ROBOT_USER: {{ required "credentials.user is required with credentials.password" .user | quote }}
      ROBOT_PASSWORD: {{ required "credentials.password is required with credentials.user" .password | quote }}
      {{- end }}
      {{- with .hcloudToken }}
      HCLOUD_TOKEN: {{ . | quote }}
      {{- end }}
{{- end }}
{{- end }}
rbac:
  roles:
    controller:
      type: ClusterRole
      # Keep the name when the leader-election role is added.
      forceRename: {{ $fullname }}
      rules:
        - apiGroups: [""]
          resources: ["nodes"]
          {{- if include "robot-ptr.resetDelay" . }}
          verbs: ["get", "list", "watch", "patch"]
          {{- else }}
          verbs: ["get", "list", "watch"]
          {{- end }}
        - apiGroups: [""]
          resources: ["pods"]
          verbs: ["get", "list", "watch"]
        - apiGroups: ["events.k8s.io"]
          resources: ["events"]
          verbs: ["create", "patch"]
    {{- if $leaderElection }}
    leader-election:
      type: Role
      rules:
        - apiGroups: ["coordination.k8s.io"]
          resources: ["leases"]
          verbs: ["create"]
        - apiGroups: ["coordination.k8s.io"]
          resources: ["leases"]
          resourceNames: ["robot-ptr.nilsit.dev"]
          verbs: ["get", "update"]
        # Leader election still records its events through the core/v1 API.
        - apiGroups: [""]
          resources: ["events"]
          verbs: ["create", "patch"]
    {{- end }}
  bindings:
    controller:
      type: ClusterRoleBinding
      forceRename: {{ $fullname }}
      roleRef:
        identifier: controller
      subjects:
        - {{ $subject | toJson }}
    {{- if $leaderElection }}
    leader-election:
      type: RoleBinding
      roleRef:
        identifier: leader-election
      subjects:
        - {{ $subject | toJson }}
    {{- end }}
{{- end -}}
