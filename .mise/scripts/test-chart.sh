#!/usr/bin/env bash
# Lints the Helm chart, validates the rendered manifests with kubeconform and
# checks the values-dependent parts: leader election, RBAC and credentials.
set -euo pipefail

chart=charts/robot-ptr
creds=(--set credentials.user=user --set credentials.password=pass)

helm dependency build "$chart" >/dev/null
helm lint --strict "$chart" "${creds[@]}"

render() {
  helm template robot-ptr "$chart" --namespace robot-ptr "$@"
}

# check <description> <yq expression over the array of all documents> <manifest>
# yq's "and" splats arrays, so each check is a single condition.
check() {
  if [[ "$(yq ea "[.] | $2" <<<"$3")" != "true" ]]; then
    echo "FAIL: $1" >&2
    exit 1
  fi
  echo "ok: $1"
}

validate() {
  kubeconform -strict -summary -kubernetes-version 1.31.0 <<<"$1"
}

args='map(select(.kind == "Deployment")) | .[0].spec.template.spec.containers[0].args'
nodeVerbs='map(select(.kind == "ClusterRole")) | .[0].rules[] | select(.resources[0] == "nodes") | .verbs'

echo "--- single replica, chart-managed secret"
m=$(render "${creds[@]}")
validate "$m"
check "no leader election flag" "$args | contains([\"--leader-elect\"]) | not" "$m"
check "no leader election role" 'map(select(.kind == "Role" or .kind == "RoleBinding")) | length == 0' "$m"
check "Recreate strategy" 'map(select(.kind == "Deployment")) | .[0].spec.strategy.type == "Recreate"' "$m"
check "service account token mounted" 'map(select(.kind == "Deployment")) | .[0].spec.template.spec.automountServiceAccountToken == true' "$m"
check "nodes patchable for pod-left-at" "$nodeVerbs | contains([\"patch\"])" "$m"
check "secret created" 'map(select(.kind == "Secret")) | .[0].stringData.ROBOT_USER == "user"' "$m"
check "secret referenced" 'map(select(.kind == "Deployment")) | .[0].spec.template.spec.containers[0].envFrom[0].secretRef.name == "robot-ptr"' "$m"
check "image tag defaults to appVersion" 'map(select(.kind == "Deployment")) | .[0].spec.template.spec.containers[0].image | test("^ghcr.io/xnilsit/robot-ptr:[0-9]")' "$m"

echo "--- several replicas, existing secret"
m=$(render --set credentials.existingSecret=robot-creds --set controllers.main.replicas=2)
validate "$m"
check "leader election flag" "$args | contains([\"--leader-elect\"])" "$m"
check "leader election role and binding" 'map(select(.kind == "Role" or .kind == "RoleBinding")) | length == 2' "$m"
check "lease updates limited to the lease name" 'map(select(.kind == "Role")) | .[0].rules[] | select(.resourceNames) | .resourceNames[0] == "robot-ptr.nilsit.dev"' "$m"
check "rolling update" 'map(select(.kind == "Deployment")) | .[0].spec.strategy.type == "RollingUpdate"' "$m"
check "cluster role keeps its name" 'map(select(.kind == "ClusterRole" or .kind == "ClusterRoleBinding")) | map(.metadata.name) | unique | .[0] == "robot-ptr"' "$m"
check "no secret created" 'map(select(.kind == "Secret")) | length == 0' "$m"
check "existing secret referenced" 'map(select(.kind == "Deployment")) | .[0].spec.template.spec.containers[0].envFrom[0].secretRef.name == "robot-creds"' "$m"

echo "--- no reset delay, custom settings"
m=$(render "${creds[@]}" --set config.resetDelay=0s --set config.dryRun=true --set config.nodeSelector=role=mail \
  --set controllers.main.strategy=RollingUpdate --set 'controllers.main.containers.main.args={--extra}')
validate "$m"
check "nodes read-only" "$nodeVerbs | contains([\"patch\"]) | not" "$m"
check "custom args" "$args | contains([\"--reset-delay=0s\", \"--dry-run\", \"--node-selector=role=mail\", \"--extra\"])" "$m"
check "user strategy kept" 'map(select(.kind == "Deployment")) | .[0].spec.strategy.type == "RollingUpdate"' "$m"

echo "--- credentials are not evaluated as templates"
m=$(render --set credentials.user=user --set-literal 'credentials.password={{ .Release.Name }}')
check "password kept verbatim" 'map(select(.kind == "Secret")) | .[0].stringData.ROBOT_PASSWORD == "{{ .Release.Name }}"' "$m"

echo "--- missing credentials fail"
if render >/dev/null 2>&1; then
  echo "FAIL: rendering without credentials succeeded" >&2
  exit 1
fi
echo "ok: rendering without credentials fails"
