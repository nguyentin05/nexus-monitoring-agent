# Nexus Monitoring Agent

Read-only AIOps service that receives metric incidents from Alertmanager, discovers novel error-log patterns in Loki, collects targeted evidence, and produces RCA through Amazon Bedrock.

## Architecture

    Prometheus rules -> Alertmanager webhook --+
                                                +-> dedup/cooldown -> exact rule -> result
    Loki error-log discovery ------------------+          |
                                                           +-> known/adaptive collection plan -> collectors -> LLM RCA
                                                           |
                                                           +-> unknown signal -> LLM planner -> collectors -> LLM RCA

`Discovery` only detects novel log patterns. Metric threshold detection belongs to Prometheus rules, avoiding duplicate incidents and duplicate LLM calls.

Collection plans are normalized read-only steps over Prometheus, Loki, Tempo, and the Kubernetes API. Supported tools are `service_metrics`, `error_logs`, `workload_status`, `kubernetes_events`, `network_policies`, `trace_context`, `recent_changes`, and `node_status`. The planner can select bounded filters and time windows, but cannot choose URLs, commands, or mutations.

Unknown-signal plans become adaptive rules only after the same normalized strategy succeeds repeatedly across services and then passes shadow validation. Active rules skip the planner call but still collect fresh evidence and run RCA. Two consecutive conclusive low-confidence results demote a rule; LLM, budget, and collector failures do not.

## Modes

| Mode | Behavior |
| --- | --- |
| `training` | Learns recurring log patterns; no LLM calls or Discord notifications |
| `shadow` | Runs analysis within budget but suppresses Discord |
| `detect` | Runs analysis and sends outcomes to Discord |

## Run

```bash
go test ./...
go run ./cmd/agent
```

The AWS SDK uses its default credential chain, including EKS IRSA.

## Configuration

| Variable | Default |
| --- | --- |
| `AGENT_MODE` | `shadow` |
| `AWS_REGION` | `ap-southeast-1` |
| `BEDROCK_MODEL_ID` | `global.amazon.nova-2-lite-v1:0` |
| `PROMETHEUS_URL` | in-cluster Prometheus service |
| `LOKI_URL` | in-cluster Loki gateway |
| `TEMPO_URL` | in-cluster Tempo service |
| `WATCHED_SERVICES` | `auth-service,profile-service` |
| `DISCOVERY_SERVICES` | `WATCHED_SERVICES` |
| `POLL_INTERVAL` | `1m` |
| `INCIDENT_COOLDOWN` | `10m` |
| `MAX_BEDROCK_CALLS_PER_HOUR` | `20`; `0` means unlimited |
| `RCA_CACHE_TTL` | `1h` |
| `PATTERN_AUTO_PROMOTE_AFTER` | `3` training observations |
| `MAX_PATTERNS` | `1000` |
| `ADAPTIVE_PLAN_MIN_OBSERVATIONS` | `5` matching planner results |
| `ADAPTIVE_PLAN_MIN_SERVICES` | `2` services |
| `ADAPTIVE_PLAN_SHADOW_MATCHES` | `3` additional successful validations |
| `STATE_DIR` | empty; state remains in memory |
| `DISCORD_WEBHOOK_URL` | empty; outcomes are only logged |

## HTTP API

- `POST /alerts`
- `GET /healthz`
- `GET /readyz`
- `GET /watched-services`
- `GET /metrics`
