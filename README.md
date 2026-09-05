# Nexus Monitoring Agent

Self-hosted Go service that correlates Alertmanager signals with Prometheus, Loki, and Kubernetes evidence before producing a bounded, read-only RCA through Amazon Bedrock.

## Architecture

    Alertmanager webhook ------------------+
                                           +--> dedup/cooldown --> exact rule --> RCA
    Prometheus metrics + Loki log polling -+          |
                                                      +--> known collection plan --> RCA cache --> LLM
                                                      |
                                                      +--> unknown signal --> LLM planner --> collectors --> LLM RCA
                                                                                |
                                                                                +--> quota/error fallback

Successful unknown-signal plans are learned in memory. A plan must repeat with
the same tools across multiple services, pass a shadow-validation period, and
produce medium/high-confidence RCA without collection errors before it becomes
active. Active plans skip only the planner call; RCA still uses current evidence.

Loki error logs also pass through a bounded pattern catalog. Dynamic values are normalized, repeated training patterns become known, and only previously unseen patterns become incidents after training. A per-service cursor prevents overlapping Loki windows from learning the same log again.

The configured LLM can only choose these read-only collectors:

- service_metrics
- error_logs
- workload_status
- kubernetes_events
- network_policies

It cannot execute commands, mutate the cluster, or choose arbitrary URLs or queries.

## Lifecycle

| Mode | Behavior |
| --- | --- |
| training | Learns recurring patterns; no LLM calls or Discord notifications |
| shadow | Runs analysis within budget and records metrics/logs, but suppresses Discord |
| detect | Runs the same pipeline and sends outcomes to Discord |

Start with training, move to shadow, then enable detect after reviewing GET /metrics and structured logs.

## Run

    go test ./...
    go run ./cmd/agent

The AWS SDK uses its default credential chain, including EKS IRSA.

## Configuration

| Variable | Default |
| --- | --- |
| AGENT_MODE | shadow |
| AWS_REGION | ap-southeast-1 |
| BEDROCK_MODEL_ID | global.amazon.nova-2-lite-v1:0 |
| PROMETHEUS_URL | in-cluster Prometheus service |
| LOKI_URL | in-cluster Loki gateway |
| WATCHED_SERVICES | auth-service,profile-service |
| DISCOVERY_SERVICES | WATCHED_SERVICES |
| POLL_INTERVAL | 1m |
| INCIDENT_COOLDOWN | 10m |
| MAX_BEDROCK_CALLS_PER_HOUR | 20; 0 means unlimited |
| RCA_CACHE_TTL | 1h |
| PATTERN_STATE_PATH | empty; in-memory catalog |
| PATTERN_AUTO_PROMOTE_AFTER | 3 observations during training |
| MAX_PATTERNS | 1000 |
| ADAPTIVE_PLAN_MIN_OBSERVATIONS | 5 matching planner results |
| ADAPTIVE_PLAN_MIN_SERVICES | 2 services |
| ADAPTIVE_PLAN_SHADOW_MATCHES | 3 additional validations |
| DISCORD_WEBHOOK_URL | empty; outcomes are logged |

Set PATTERN_STATE_PATH=/data/patterns.json and mount /data to preserve learned patterns and Loki cursors across Pod restarts.

## HTTP API

- POST /alerts
- POST /trigger
- GET /healthz
- GET /readyz
- GET /watched-services
- GET /metrics
