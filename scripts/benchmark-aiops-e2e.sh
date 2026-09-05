#!/usr/bin/env bash
set -Eeuo pipefail

NAMESPACE="apps"
MONITORING_NAMESPACE="monitoring"
SERVICE="aiops-benchmark-service"
REPEAT=1
POLL_SECONDS=5
TIMEOUT_SECONDS=300
COOLDOWN_SECONDS=30
SCENARIO=""
PREFLIGHT_ONLY=false
OUTPUT=""
APP_PORT=18081
AGENT_PORT=18080
PROMETHEUS_PORT=19090

usage() {
  cat <<'EOF'
Usage: scripts/benchmark-aiops-e2e.sh [options]

Options:
  --repeat N             Repetitions per scenario (default: 1)
  --scenario ID          Run one scenario only, for example A06
  --poll-seconds N       Poll interval (default: 5)
  --timeout-seconds N    Timeout per scenario (default: 300)
  --cooldown-seconds N   Pause after cleanup (default: 30)
  --output FILE          JSONL result path
  --preflight            Check prerequisites without mutating the cluster
  -h, --help             Show this help

This suite creates temporary resources in apps and monitoring. It injects
real faults, waits for Prometheus/Alertmanager, the deployed AIOps agent,
Bedrock RCA, and a successful Discord webhook response, then cleans up.
EOF
}

while (($#)); do
  case "$1" in
    --repeat) REPEAT="$2"; shift 2 ;;
    --scenario) SCENARIO="$2"; shift 2 ;;
    --poll-seconds) POLL_SECONDS="$2"; shift 2 ;;
    --timeout-seconds) TIMEOUT_SECONDS="$2"; shift 2 ;;
    --cooldown-seconds) COOLDOWN_SECONDS="$2"; shift 2 ;;
    --output) OUTPUT="$2"; shift 2 ;;
    --preflight) PREFLIGHT_ONLY=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

for value in "$REPEAT" "$POLL_SECONDS" "$TIMEOUT_SECONDS"; do
  [[ "$value" =~ ^[1-9][0-9]*$ ]] || { echo "Numeric options must be positive integers" >&2; exit 2; }
done
[[ "$COOLDOWN_SECONDS" =~ ^[0-9]+$ ]] || { echo "--cooldown-seconds must be non-negative" >&2; exit 2; }
[[ -z "$SCENARIO" || "$SCENARIO" =~ ^A(0[1-9]|1[0-5])$ ]] || { echo "--scenario must be A01..A15" >&2; exit 2; }

ROOT="$(git rev-parse --show-toplevel 2>/dev/null || true)"
[[ -n "$ROOT" && -d "$ROOT/internal/agent" ]] || { echo "Run this script from nexus-monitoring-agent" >&2; exit 1; }
cd "$ROOT"

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
OUTPUT="${OUTPUT:-benchmark-results/aiops-e2e-$STAMP.jsonl}"
CSV="${OUTPUT%.jsonl}.csv"
SUMMARY="${OUTPUT%.jsonl}.summary.md"
PORT_FORWARD_PIDS=()
ACTIVE_RUN_ID=""

log() { printf '[%s] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
die() { echo "ERROR: $*" >&2; exit 1; }
now_ms() { date +%s%3N; }

wait_for() {
  local description="$1" predicate="$2" started
  started="$(date +%s)"
  until eval "$predicate"; do
    (( $(date +%s) - started < TIMEOUT_SECONDS )) || return 1
    sleep "$POLL_SECONDS"
  done
  log "$description"
}

preflight() {
  log "Running end-to-end AIOps benchmark preflight"
  for command in kubectl curl jq python3 git; do
    command -v "$command" >/dev/null || die "Missing command: $command"
  done
  kubectl get deployment auth-service -n "$NAMESPACE" >/dev/null || die "auth-service is unavailable"
  kubectl get deployment monitoring-agent -n "$MONITORING_NAMESPACE" >/dev/null || die "monitoring-agent is unavailable"
  kubectl get prometheus monitoring-prometheus -n "$MONITORING_NAMESPACE" >/dev/null || die "Prometheus is unavailable"
  kubectl get alertmanager monitoring-alertmanager -n "$MONITORING_NAMESPACE" >/dev/null || die "Alertmanager is unavailable"
  for resource in deployments pods services networkpolicies resourcequotas; do
    kubectl auth can-i create "$resource" -n "$NAMESPACE" | grep -Fxq yes || die "Cannot create $resource in $NAMESPACE"
    kubectl auth can-i delete "$resource" -n "$NAMESPACE" | grep -Fxq yes || die "Cannot delete $resource in $NAMESPACE"
  done
  kubectl auth can-i create servicemonitors.monitoring.coreos.com -n "$NAMESPACE" | grep -Fxq yes || die "Cannot create ServiceMonitor"
  kubectl auth can-i create prometheusrules.monitoring.coreos.com -n "$MONITORING_NAMESPACE" | grep -Fxq yes || die "Cannot create PrometheusRule"
  kubectl auth can-i create horizontalpodautoscalers.autoscaling -n "$NAMESPACE" | grep -Fxq yes || die "Cannot create HPA"
  kubectl auth can-i create externalsecrets.external-secrets.io -n "$NAMESPACE" | grep -Fxq yes || die "Cannot create ExternalSecret"
  image="$(kubectl get deployment auth-service -n "$NAMESPACE" -o jsonpath='{.spec.template.spec.containers[0].image}')"
  [[ -n "$image" ]] || die "Cannot resolve signed auth-service image"
  watched="$(kubectl get deployment monitoring-agent -n "$MONITORING_NAMESPACE" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="WATCHED_SERVICES")].value}')"
  [[ ",$watched," == *",$SERVICE,"* ]] || die "$SERVICE must be accepted by WATCHED_SERVICES"
  discovery="$(kubectl get deployment monitoring-agent -n "$MONITORING_NAMESPACE" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="DISCOVERY_SERVICES")].value}')"
  [[ ",$discovery," != *",$SERVICE,"* ]] || die "$SERVICE must be excluded from DISCOVERY_SERVICES so polling does not duplicate the controlled Alertmanager incident"
  budget="$(kubectl get deployment monitoring-agent -n "$MONITORING_NAMESPACE" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="MAX_BEDROCK_CALLS_PER_HOUR")].value}')"
  [[ "$budget" =~ ^[0-9]+$ ]] || die "Cannot read deployed MAX_BEDROCK_CALLS_PER_HOUR"
  scenario_count=15
  [[ -z "$SCENARIO" ]] || scenario_count=1
  planned_calls=$((REPEAT * scenario_count * 2))
  [[ "$budget" == "0" || "$budget" -ge "$planned_calls" ]] || die "Suite needs up to $planned_calls Bedrock calls but deployed hourly budget is $budget"
  log "Preflight passed; benchmark image: $image; planned Bedrock calls: $planned_calls"
}

delete_case_resources() {
  kubectl delete deployment,pod,networkpolicy,resourcequota,horizontalpodautoscaler,externalsecret \
    -n "$NAMESPACE" -l benchmark.nexus/case=true --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl scale deployment "$SERVICE" -n "$NAMESPACE" --replicas=1 >/dev/null 2>&1 || true
}

cleanup() {
  for pid in "${PORT_FORWARD_PIDS[@]}"; do kill "$pid" >/dev/null 2>&1 || true; done
  delete_case_resources
  kubectl delete prometheusrule -n "$MONITORING_NAMESPACE" aiops-benchmark --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete servicemonitor,service,deployment -n "$NAMESPACE" \
    -l benchmark.nexus/suite=true --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

setup_suite() {
  image="$(kubectl get deployment auth-service -n "$NAMESPACE" -o jsonpath='{.spec.template.spec.containers[0].image}')"
  kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $SERVICE
  namespace: $NAMESPACE
  labels: {benchmark.nexus/suite: "true"}
spec:
  replicas: 1
  selector:
    matchLabels: {app.kubernetes.io/name: $SERVICE}
  template:
    metadata:
      labels:
        app.kubernetes.io/name: $SERVICE
        benchmark.nexus/suite: "true"
    spec:
      nodeSelector: {intent: apps}
      securityContext: {runAsNonRoot: true, runAsUser: 10001, runAsGroup: 10001, seccompProfile: {type: RuntimeDefault}}
      containers:
        - name: $SERVICE
          image: $image
          imagePullPolicy: IfNotPresent
          securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: ["ALL"]}}
          env:
            - {name: AIOPS_BENCHMARK_ENABLED, value: "true"}
            - {name: OTEL_SERVICE_NAME, value: "$SERVICE"}
            - {name: OTEL_EXPORTER_OTLP_ENDPOINT, value: "http://opentelemetry-collector.monitoring.svc.cluster.local:4318"}
            - {name: OTEL_EXPORTER_OTLP_PROTOCOL, value: "http/protobuf"}
          ports: [{name: http, containerPort: 8000}]
          readinessProbe: {httpGet: {path: /health, port: http}, periodSeconds: 5}
          resources:
            requests: {cpu: 100m, memory: 96Mi}
            limits: {cpu: "1", memory: 256Mi}
          volumeMounts: [{name: tmp, mountPath: /tmp}]
      volumes: [{name: tmp, emptyDir: {}}]
---
apiVersion: v1
kind: Service
metadata:
  name: $SERVICE
  namespace: $NAMESPACE
  labels:
    app.kubernetes.io/name: $SERVICE
    benchmark.nexus/suite: "true"
spec:
  selector: {app.kubernetes.io/name: $SERVICE, benchmark.nexus/suite: "true"}
  ports: [{name: http, port: 80, targetPort: http}]
---
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: $SERVICE
  namespace: $NAMESPACE
  labels: {benchmark.nexus/suite: "true"}
spec:
  selector:
    matchLabels: {app.kubernetes.io/name: $SERVICE}
  endpoints: [{port: http, path: /metrics, interval: 10s}]
---
apiVersion: v1
kind: Service
metadata:
  name: aiops-benchmark-dependency
  namespace: $NAMESPACE
  labels: {benchmark.nexus/suite: "true"}
spec:
  selector: {app.kubernetes.io/name: $SERVICE, benchmark.nexus/suite: "true"}
  ports: [{name: http, port: 8000, targetPort: http}]
---
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: aiops-benchmark
  namespace: $MONITORING_NAMESPACE
spec:
  groups:
    - name: nexus.aiops.benchmark
      interval: 10s
      rules:
        - alert: AIOpsBenchmarkIncident
          expr: nexus_aiops_benchmark_fault{namespace="$NAMESPACE",service="$SERVICE"} == 1
          for: 10s
          labels:
            severity: critical
            namespace: $NAMESPACE
            service: $SERVICE
          annotations:
            summary: "Controlled benchmark incident {{ \$labels.run_id }}"
            description: "{{ \$labels.symptom }}"
EOF
  kubectl rollout status deployment/"$SERVICE" -n "$NAMESPACE" --timeout=180s >/dev/null
}

start_port_forwards() {
  kubectl port-forward -n "$NAMESPACE" service/"$SERVICE" "$APP_PORT:80" >/tmp/nexus-aiops-app-pf.log 2>&1 &
  PORT_FORWARD_PIDS+=("$!")
  kubectl port-forward -n "$MONITORING_NAMESPACE" service/monitoring-agent "$AGENT_PORT:8080" >/tmp/nexus-aiops-agent-pf.log 2>&1 &
  PORT_FORWARD_PIDS+=("$!")
  kubectl port-forward -n "$MONITORING_NAMESPACE" service/monitoring-prometheus "$PROMETHEUS_PORT:9090" >/tmp/nexus-aiops-prom-pf.log 2>&1 &
  PORT_FORWARD_PIDS+=("$!")
  wait_for "Port forwards ready" "curl -fsS http://127.0.0.1:$APP_PORT/health >/dev/null && curl -fsS http://127.0.0.1:$AGENT_PORT/readyz >/dev/null && curl -fsS http://127.0.0.1:$PROMETHEUS_PORT/-/ready >/dev/null" ||
    die "Port-forward setup failed"
  curl -fsS --max-time 10 -X POST --get "http://127.0.0.1:$APP_PORT/_benchmark/clear" --data-urlencode "scenario=preflight" --data-urlencode "run_id=preflight" >/dev/null || die "Deployed auth-service image does not expose benchmark endpoints"
  wait_for "Prometheus is scraping the benchmark service" "curl -fsS --get http://127.0.0.1:$PROMETHEUS_PORT/api/v1/query --data-urlencode 'query=up{namespace=\"apps\",service=\"aiops-benchmark-service\"} == 1' | jq -e '.data.result | length > 0' >/dev/null" || die "Prometheus is not scraping the benchmark ServiceMonitor"
}

app_get() {
  curl -sS --max-time 70 "http://127.0.0.1:$APP_PORT/_benchmark/$1?run_id=$ACTIVE_RUN_ID${2:-}" >/dev/null || true
}

arm_fault() {
  curl -fsS --max-time 10 -X POST --get "http://127.0.0.1:$APP_PORT/_benchmark/arm" \
    --data-urlencode "scenario=$1" --data-urlencode "run_id=$ACTIVE_RUN_ID" --data-urlencode "symptom=$2" >/dev/null
}

clear_fault() {
  curl -fsS --max-time 10 -X POST --get "http://127.0.0.1:$APP_PORT/_benchmark/clear" \
    --data-urlencode "scenario=$1" --data-urlencode "run_id=$ACTIVE_RUN_ID" >/dev/null || true
}

fault_pod() {
  local name="$1" command="$2" memory="${3:-64Mi}" ephemeral="${4:-64Mi}"
  kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $name
  namespace: $NAMESPACE
  labels:
    app.kubernetes.io/name: $SERVICE
    benchmark.nexus/case: "true"
spec:
  restartPolicy: Always
  containers:
    - name: $SERVICE
      image: $image
      command: ["python", "-c", "$command"]
      resources:
        requests: {cpu: 10m, memory: 16Mi}
        limits: {cpu: 100m, memory: $memory, ephemeral-storage: $ephemeral}
      volumeMounts: [{name: scratch, mountPath: /tmp/benchmark}]
  volumes: [{name: scratch, emptyDir: {sizeLimit: $ephemeral}}]
EOF
}

inject() {
  local id="$1"
  case "$id" in
    A01) for _ in {1..20}; do app_get error; done; SYMPTOM="HTTP 5xx rate spiked while traffic remained stable" ;;
    A02) app_get cpu "&seconds=45" & FAULT_PID=$!; sleep 20; SYMPTOM="CPU usage and request latency exceeded their thresholds" ;;
    A03) fault_pod "$SERVICE-oom-$ACTIVE_RUN_ID" "x=[]\nwhile True: x.append(bytearray(1048576))" 32Mi; sleep 20; SYMPTOM="A workload container restarted after rapid memory growth" ;;
    A04) for _ in {1..5}; do app_get database-refused; done; SYMPTOM="Persistence operations fail immediately while health checks pass" ;;
    A05) for _ in {1..8}; do app_get slow "&seconds=2"; done; SYMPTOM="The service p99 response time exceeded 1.5 seconds without CPU saturation" ;;
    A06) for _ in {1..5}; do app_get dns; done; SYMPTOM="An internal dependency fails before a connection can be established" ;;
    A07) for _ in {1..5}; do app_get certificate; done; SYMPTOM="TLS requests fail while plain TCP connectivity remains available" ;;
    A08) for _ in {1..5}; do app_get dependency-timeout; done; SYMPTOM="An upstream consumes the full client timeout and returns 504" ;;
    A09) fault_pod "$SERVICE-disk-$ACTIVE_RUN_ID" "p=open('/tmp/benchmark/fill','wb')\nwhile True: p.write(b'x'*1048576); p.flush()" 64Mi 16Mi; sleep 20; SYMPTOM="A workload failed while node-local ephemeral storage was being consumed" ;;
    A10)
      kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $SERVICE-image-$ACTIVE_RUN_ID
  namespace: $NAMESPACE
  labels: {app.kubernetes.io/name: $SERVICE, benchmark.nexus/case: "true"}
spec:
  containers: [{name: $SERVICE, image: invalid.example/nexus/missing:$ACTIVE_RUN_ID}]
EOF
      sleep 15; SYMPTOM="A newly requested workload remains unavailable before its container starts"
      ;;
    A11)
      kubectl apply -f - <<EOF
apiVersion: v1
kind: ResourceQuota
metadata:
  name: $SERVICE-quota-$ACTIVE_RUN_ID
  namespace: $NAMESPACE
  labels: {benchmark.nexus/case: "true"}
spec:
  scopes: [BestEffort]
  hard: {pods: "0"}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $SERVICE-quota-$ACTIVE_RUN_ID
  namespace: $NAMESPACE
  labels: {benchmark.nexus/case: "true"}
spec:
  replicas: 1
  selector: {matchLabels: {benchmark.nexus/quota-run: "$ACTIVE_RUN_ID"}}
  template:
    metadata: {labels: {benchmark.nexus/quota-run: "$ACTIVE_RUN_ID"}}
    spec:
      containers: [{name: benchmark, image: invalid.example/nexus/quota:$ACTIVE_RUN_ID}]
EOF
      sleep 15; SYMPTOM="A controller cannot create its requested pod in the namespace"
      ;;
    A12)
      kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $SERVICE-liveness-$ACTIVE_RUN_ID
  namespace: $NAMESPACE
  labels: {app.kubernetes.io/name: $SERVICE, benchmark.nexus/case: "true"}
spec:
  containers:
    - name: $SERVICE
      image: $image
      env: [{name: AIOPS_BENCHMARK_ENABLED, value: "true"}]
      ports: [{name: http, containerPort: 8000}]
      livenessProbe:
        httpGet: {path: "/_benchmark/error?run_id=$ACTIVE_RUN_ID", port: http}
        initialDelaySeconds: 2
        periodSeconds: 3
        failureThreshold: 1
      resources:
        requests: {cpu: 10m, memory: 64Mi}
        limits: {cpu: 100m, memory: 128Mi}
EOF
      sleep 20; SYMPTOM="A running container repeatedly restarts and never remains healthy"
      ;;
    A13)
      kubectl apply -f - <<EOF
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: $SERVICE-deny-$ACTIVE_RUN_ID
  namespace: $NAMESPACE
  labels: {benchmark.nexus/case: "true"}
spec:
  podSelector: {matchLabels: {app.kubernetes.io/name: $SERVICE}}
  policyTypes: [Egress]
  egress:
    - to:
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: kube-system}}
          podSelector: {matchLabels: {k8s-app: kube-dns}}
      ports: [{protocol: UDP, port: 53}, {protocol: TCP, port: 53}]
EOF
      app_get network; SYMPTOM="A resolvable internal dependency became unreachable after a policy change"
      ;;
    A14)
      kubectl autoscale deployment "$SERVICE" -n "$NAMESPACE" --cpu-percent=20 --min=1 --max=4 >/dev/null
      kubectl label hpa "$SERVICE" -n "$NAMESPACE" benchmark.nexus/case=true --overwrite >/dev/null
      for _ in 1 2; do app_get cpu "&seconds=45" & sleep 50; sleep 30; done
      SYMPTOM="Replica count repeatedly oscillates while request volume is periodic"
      ;;
    A15)
      kubectl apply -f - <<EOF
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: $SERVICE-secret-$ACTIVE_RUN_ID
  namespace: $NAMESPACE
  labels: {benchmark.nexus/case: "true"}
spec:
  refreshInterval: 10s
  secretStoreRef: {name: vault-backend, kind: SecretStore}
  target: {name: $SERVICE-missing-$ACTIVE_RUN_ID}
  dataFrom: [{extract: {key: benchmark/does-not-exist-$ACTIVE_RUN_ID}}]
EOF
      sleep 20; SYMPTOM="A rollout cannot obtain required credentials after secret reconciliation"
      ;;
  esac
}

prometheus_firing() {
  curl -fsS --get "http://127.0.0.1:$PROMETHEUS_PORT/api/v1/query" \
    --data-urlencode "query=ALERTS{alertname=\"AIOpsBenchmarkIncident\",run_id=\"$ACTIVE_RUN_ID\",alertstate=\"firing\"}" |
    jq -e '.data.result | length > 0' >/dev/null
}

processed_line() {
  kubectl logs -n "$MONITORING_NAMESPACE" deployment/monitoring-agent --since-time="$CASE_STARTED_ISO" 2>/dev/null |
    grep 'incident processed' | grep 'aiopsbenchmarkincident' | grep "$ACTIVE_RUN_ID" | tail -1
}

notification_line() {
  kubectl logs -n "$MONITORING_NAMESPACE" deployment/monitoring-agent --since-time="$CASE_STARTED_ISO" 2>/dev/null |
    grep 'incident notification sent' | grep 'aiopsbenchmarkincident' | grep "$ACTIVE_RUN_ID" | tail -1
}

metric_value() {
  curl -fsS "http://127.0.0.1:$AGENT_PORT/metrics" |
    awk -v metric="$1" '$1 == metric {sum += $2} END {print sum + 0}'
}

record_result() {
  local id="$1" fault_ready_ms="$2" alert_ms="$3" notify_ms="$4" line="$5"
  local before_input="$6" before_output="$7" before_planner="$8" before_rca="$9"
  local after_input after_output after_planner after_rca
  after_input="$(metric_value 'nexus_agent_bedrock_tokens_total{type="input"}')"
  after_output="$(metric_value 'nexus_agent_bedrock_tokens_total{type="output"}')"
  after_planner="$(metric_value 'nexus_agent_bedrock_calls_total{stage="planner"}')"
  after_rca="$(metric_value 'nexus_agent_bedrock_calls_total{stage="rca"}')"
  jq -cn \
    --arg run_id "$ACTIVE_RUN_ID" --arg scenario "$id" --arg symptom "$SYMPTOM" \
    --arg processed_log "$line" --argjson started_ms "$CASE_STARTED_MS" \
    --argjson fault_ready_ms "$fault_ready_ms" --argjson alert_ms "$alert_ms" --argjson notified_ms "$notify_ms" \
    --argjson input_before "$before_input" --argjson output_before "$before_output" \
    --argjson input_after "$after_input" --argjson output_after "$after_output" \
    --argjson planner_before "$before_planner" --argjson planner_after "$after_planner" \
    --argjson rca_before "$before_rca" --argjson rca_after "$after_rca" \
    '{run_id:$run_id,scenario:$scenario,symptom:$symptom,started_ms:$started_ms,
      fault_ready_ms:$fault_ready_ms,alert_firing_ms:$alert_ms,notified_ms:$notified_ms,
      fault_setup_s:(($fault_ready_ms-$started_ms)/1000),
      detection_s:(($alert_ms-$fault_ready_ms)/1000),
      aiops_s:(($notified_ms-$alert_ms)/1000),
      end_to_end_s:(($notified_ms-$started_ms)/1000),
      pipeline_end_to_end_s:(($notified_ms-$fault_ready_ms)/1000),
      processed_log:$processed_log,
      planner_calls:($planner_after-$planner_before),
      rca_calls:($rca_after-$rca_before),
      input_tokens:($input_after-$input_before),
      output_tokens:($output_after-$output_before),
      result:"success"}' >> "$OUTPUT"
}

record_failure() {
  local id="$1" stage="$2" message="$3" fault_ready_ms="${4:-0}"
  jq -cn \
    --arg run_id "$ACTIVE_RUN_ID" --arg scenario "$id" --arg symptom "${SYMPTOM:-}" \
    --arg fail_stage "$stage" --arg error "$message" \
    --argjson started_ms "$CASE_STARTED_MS" --argjson fault_ready_ms "$fault_ready_ms" \
    '{run_id:$run_id,scenario:$scenario,symptom:$symptom,started_ms:$started_ms,
      fault_ready_ms:$fault_ready_ms,fail_stage:$fail_stage,error:$error,result:"fail"}' >> "$OUTPUT"
}

finish_case() {
  clear_fault "${1:-unknown}"
  delete_case_resources
  sleep "$COOLDOWN_SECONDS"
}

run_case() {
  local id="$1" before_input before_output before_planner before_rca line fault_ready_ms alert_ms notify_ms
  ACTIVE_RUN_ID="$(printf '%s-r%02d-%s' "${id,,}" "$2" "$(date -u +%H%M%S)")"
  CASE_STARTED_ISO="$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)"
  CASE_STARTED_MS="$(now_ms)"
  before_input="$(metric_value 'nexus_agent_bedrock_tokens_total{type="input"}')"
  before_output="$(metric_value 'nexus_agent_bedrock_tokens_total{type="output"}')"
  before_planner="$(metric_value 'nexus_agent_bedrock_calls_total{stage="planner"}')"
  before_rca="$(metric_value 'nexus_agent_bedrock_calls_total{stage="rca"}')"
  log "$ACTIVE_RUN_ID: injecting fault"
  if ! inject "$id"; then
    record_failure "$id" injection "fault injection command failed"
    finish_case "$id"
    return
  fi
  fault_ready_ms="$(now_ms)"
  if ! arm_fault "$id" "$SYMPTOM"; then
    record_failure "$id" trigger "benchmark metric could not be armed" "$fault_ready_ms"
    finish_case "$id"
    return
  fi
  if ! wait_for "$ACTIVE_RUN_ID: Prometheus alert firing" prometheus_firing; then
    record_failure "$id" detection "Prometheus alert did not fire" "$fault_ready_ms"
    finish_case "$id"
    return
  fi
  alert_ms="$(now_ms)"
  if ! wait_for "$ACTIVE_RUN_ID: agent processed incident" "test -n \"\$(processed_line)\""; then
    record_failure "$id" rca "agent did not process the Alertmanager incident" "$fault_ready_ms"
    finish_case "$id"
    return
  fi
  line="$(processed_line)"
  if ! wait_for "$ACTIVE_RUN_ID: Discord webhook accepted" "test -n \"\$(notification_line)\""; then
    record_failure "$id" notification "Discord notification was not confirmed" "$fault_ready_ms"
    finish_case "$id"
    return
  fi
  notify_ms="$(now_ms)"
  record_result "$id" "$fault_ready_ms" "$alert_ms" "$notify_ms" "$line" "$before_input" "$before_output" "$before_planner" "$before_rca"
  finish_case "$id"
}

write_reports() {
  python3 - "$OUTPUT" "$CSV" "$SUMMARY" "$ROOT/benchmark/rca-scenarios.json" <<'PY'
import csv
import json
import math
import shlex
import statistics
import sys

raw, csv_path, summary_path, fixture_path = sys.argv[1:]
with open(raw, encoding="utf-8") as handle:
    rows = [json.loads(line) for line in handle if line.strip()]
with open(fixture_path, encoding="utf-8") as handle:
    fixtures = {item["id"]: item for item in json.load(handle)}


def log_fields(line):
    parsed = {}
    for token in shlex.split(line):
        if "=" in token:
            key, value = token.split("=", 1)
            parsed[key] = value
    return parsed


for row in rows:
    fixture = fixtures[row["scenario"]]
    parsed = log_fields(row.get("processed_log", ""))
    root_cause = parsed.get("root_cause", "")
    root_lower = root_cause.lower()
    row["scenario_name"] = fixture["name"]
    row["ground_truth"] = fixture["ground_truth"]
    row["root_cause"] = root_cause
    row["path"] = parsed.get("path", "")
    row["confidence"] = parsed.get("confidence", "")
    row["fallback"] = parsed.get("fallback", "false") == "true"
    row["rca_concept_pass"] = all(
        any(word in root_lower for word in group)
        for group in fixture["expected_concepts"]
    )
    if row.get("processed_log"):
        row["result"] = "pass" if row["rca_concept_pass"] and not row["fallback"] else "fail"

fields = [
    "run_id", "scenario", "scenario_name", "symptom", "ground_truth", "result", "fail_stage", "error",
    "rca_concept_pass", "manual_rca_score", "path", "root_cause", "confidence", "fallback",
    "fault_setup_s", "detection_s", "aiops_s", "pipeline_end_to_end_s", "end_to_end_s",
    "planner_calls", "rca_calls", "input_tokens", "output_tokens", "processed_log",
]
with open(csv_path, "w", newline="", encoding="utf-8") as handle:
    writer = csv.DictWriter(handle, fieldnames=fields)
    writer.writeheader()
    writer.writerows({field: row.get(field, "") for field in fields} for row in rows)


def p95(values):
    values = sorted(values)
    return values[max(0, math.ceil(len(values) * .95) - 1)]


completed = [row for row in rows if row.get("processed_log")]
passed = [row for row in rows if row["result"] == "pass"]
lines = [
    "# AIOps End-to-End Benchmark", "",
    f"- Attempted cases: {len(rows)}",
    f"- End-to-end completion rate: {len(completed) / len(rows) * 100 if rows else 0:.2f}%",
    f"- Preliminary RCA concept accuracy: {len(passed) / len(completed) * 100 if completed else 0:.2f}%",
    f"- Fallback rate: {sum(row.get('fallback', False) for row in completed) / len(completed) * 100 if completed else 0:.2f}%",
    f"- Total planner calls: {sum(row.get('planner_calls', 0) for row in rows)}",
    f"- Total RCA calls: {sum(row.get('rca_calls', 0) for row in rows)}",
    f"- Total input tokens: {sum(row.get('input_tokens', 0) for row in rows)}",
    f"- Total output tokens: {sum(row.get('output_tokens', 0) for row in rows)}",
]
for field, label in (
    ("detection_s", "Detection"),
    ("aiops_s", "AIOps"),
    ("pipeline_end_to_end_s", "Pipeline end-to-end"),
):
    values = [float(row[field]) for row in completed if field in row]
    if values:
        lines += [
            f"- Median {label} latency: {statistics.median(values):.3f} s",
            f"- P95 {label} latency: {p95(values):.3f} s",
        ]
lines += [
    "", "## Interpretation", "",
    "Detection starts after fault setup and ends when the benchmark alert is observed firing.",
    "The Prometheus rule is a controlled trigger; this suite evaluates transport, evidence collection, RCA and notification, not detector-rule accuracy.",
    "RCA concept accuracy is preliminary. Fill manual_rca_score in the CSV with 0, 1, or 2 before reporting final accuracy.",
]
with open(summary_path, "w", encoding="utf-8") as handle:
    handle.write("\n".join(lines) + "\n")
print("\n".join(lines))
PY
}

preflight
[[ "$PREFLIGHT_ONLY" == false ]] || exit 0
mkdir -p "$(dirname "$OUTPUT")"
: > "$OUTPUT"
setup_suite
start_port_forwards

scenarios=(A01 A02 A03 A04 A05 A06 A07 A08 A09 A10 A11 A12 A13 A14 A15)
[[ -z "$SCENARIO" ]] || scenarios=("$SCENARIO")
for run in $(seq 1 "$REPEAT"); do
  for id in "${scenarios[@]}"; do run_case "$id" "$run"; done
done
write_reports
log "JSONL: $OUTPUT"
log "CSV: $CSV"
log "Summary: $SUMMARY"
