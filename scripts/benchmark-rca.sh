#!/usr/bin/env bash
set -Eeuo pipefail

REPEAT=1
COOLDOWN_SECONDS=5
TIMEOUT_SECONDS=90
WARMUP=false
PREFLIGHT_ONLY=false
SCENARIO=""
OUTPUT=""

usage() {
  cat <<'EOF'
Usage: scripts/benchmark-rca.sh [options]

Options:
  --repeat N             Measured repetitions per scenario (default: 1)
  --scenario ID          Run one scenario only, for example A06
  --warmup               Run the first selected scenario once and exclude it
  --cooldown-seconds N   Pause between Bedrock cases (default: 5)
  --timeout-seconds N    Timeout for each planner/RCA call (default: 90)
  --output FILE          JSONL output path
  --preflight            Validate tools, fixtures, tests and AWS identity only
  -h, --help             Show this help

The benchmark replays synthetic evidence and invokes Bedrock for real. Each
measured scenario normally makes two model calls: planner and RCA.
EOF
}

while (($#)); do
  case "$1" in
    --repeat) REPEAT="$2"; shift 2 ;;
    --scenario) SCENARIO="$2"; shift 2 ;;
    --warmup) WARMUP=true; shift ;;
    --cooldown-seconds) COOLDOWN_SECONDS="$2"; shift 2 ;;
    --timeout-seconds) TIMEOUT_SECONDS="$2"; shift 2 ;;
    --output) OUTPUT="$2"; shift 2 ;;
    --preflight) PREFLIGHT_ONLY=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ "$REPEAT" =~ ^[1-9][0-9]*$ ]] || { echo "--repeat must be a positive integer" >&2; exit 2; }
[[ "$COOLDOWN_SECONDS" =~ ^[0-9]+$ ]] || { echo "--cooldown-seconds must be a non-negative integer" >&2; exit 2; }
[[ "$TIMEOUT_SECONDS" =~ ^[1-9][0-9]*$ ]] || { echo "--timeout-seconds must be a positive integer" >&2; exit 2; }

ROOT="$(git rev-parse --show-toplevel 2>/dev/null || true)"
[[ -n "$ROOT" && -f "$ROOT/go.mod" && -d "$ROOT/internal/agent" ]] || {
  echo "Run this script from nexus-monitoring-agent" >&2
  exit 1
}
cd "$ROOT"

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
OUTPUT="${OUTPUT:-benchmark-results/rca-$STAMP.jsonl}"
CSV="${OUTPUT%.jsonl}.csv"
SUMMARY="${OUTPUT%.jsonl}.summary.md"
FIXTURES="benchmark/rca-scenarios.json"

log() { printf '[%s] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
die() { echo "ERROR: $*" >&2; exit 1; }

preflight() {
  log "Running RCA benchmark preflight"
  for command in aws go jq python3 git; do
    command -v "$command" >/dev/null || die "Missing command: $command"
  done
  jq -e 'type == "array" and length == 15 and all(.[]; .id and .ground_truth and (.expected_concepts | length > 0))' "$FIXTURES" >/dev/null ||
    die "RCA fixture file is invalid"
  if [[ -n "$SCENARIO" ]]; then
    jq -e --arg id "$SCENARIO" 'any(.[]; .id == $id)' "$FIXTURES" >/dev/null ||
      die "Unknown scenario: $SCENARIO"
  fi
  aws sts get-caller-identity >/dev/null || die "AWS credential chain is unavailable"
  go test ./... >/dev/null || die "Go tests failed"
  log "Preflight passed: fixtures, Go tests and AWS credentials are ready"
}

write_reports() {
  python3 - "$OUTPUT" "$CSV" "$SUMMARY" <<'PY'
import csv
import json
import math
import statistics
import sys
from collections import defaultdict

raw_path, csv_path, summary_path = sys.argv[1:]
with open(raw_path, encoding="utf-8") as handle:
    rows = [json.loads(line) for line in handle if line.strip()]

columns = [
    "run", "warmup", "scenario_id", "scenario", "category", "ground_truth",
    "required_tools", "selected_tools", "planner_pass", "rca_pass",
    "planner_minimal", "manual_rca_score", "path", "root_cause", "confidence", "evidence",
    "suggested_actions", "fallback", "error", "latency_ms", "planner_calls",
    "rca_calls", "input_tokens", "output_tokens", "cache_read_tokens",
    "cache_write_tokens", "result",
]
with open(csv_path, "w", newline="", encoding="utf-8") as handle:
    writer = csv.DictWriter(handle, fieldnames=columns)
    writer.writeheader()
    for row in rows:
        output = dict(row)
        for field in ("required_tools", "selected_tools", "evidence", "suggested_actions"):
            output[field] = json.dumps(output.get(field, []), ensure_ascii=False)
        output["planner_minimal"] = set(row.get("required_tools", [])) == set(row.get("selected_tools", []))
        output["manual_rca_score"] = ""
        output["result"] = "pass" if (
            row.get("path") == "llm_planner"
            and row.get("planner_pass")
            and row.get("rca_pass")
            and not row.get("fallback")
            and row.get("planner_calls") == 1
            and row.get("rca_calls") == 1
        ) else "fail"
        writer.writerow({column: output.get(column, "") for column in columns})

measured = [row for row in rows if not row.get("warmup")]
passed = [
    row for row in measured
    if row.get("path") == "llm_planner"
    and row.get("planner_pass")
    and row.get("rca_pass")
    and not row.get("fallback")
    and row.get("planner_calls") == 1
    and row.get("rca_calls") == 1
]
latencies = [row["latency_ms"] / 1000 for row in measured]

def percentage(count, total):
    return count / total * 100 if total else 0

def percentile(values, quantile):
    ordered = sorted(values)
    return ordered[max(0, math.ceil(len(ordered) * quantile) - 1)]

lines = [
    "# AIOps RCA Benchmark",
    "",
    f"- Total observations: {len(rows)}",
    f"- Warm-up observations excluded: {len(rows) - len(measured)}",
    f"- Measured observations: {len(measured)}",
    f"- Pipeline pass rate: {percentage(len(passed), len(measured)):.2f}%",
    f"- Planner required-tool coverage: {percentage(sum(bool(r.get('planner_pass')) for r in measured), len(measured)):.2f}%",
    f"- Planner minimal-tool rate: {percentage(sum(set(r.get('required_tools', [])) == set(r.get('selected_tools', [])) for r in measured), len(measured)):.2f}%",
    f"- Preliminary RCA concept accuracy: {percentage(sum(bool(r.get('rca_pass')) for r in measured), len(measured)):.2f}%",
    f"- Fallback rate: {percentage(sum(bool(r.get('fallback')) for r in measured), len(measured)):.2f}%",
]
if latencies:
    lines += [
        f"- Mean pipeline latency: {statistics.mean(latencies):.3f} s",
        f"- Median pipeline latency: {statistics.median(latencies):.3f} s",
        f"- P95 pipeline latency: {percentile(latencies, 0.95):.3f} s",
        f"- Mean input tokens: {statistics.mean(r['input_tokens'] for r in measured):.1f}",
        f"- Mean output tokens: {statistics.mean(r['output_tokens'] for r in measured):.1f}",
    ]

by_scenario = defaultdict(list)
for row in measured:
    by_scenario[row["scenario_id"]].append(row)
if by_scenario:
    lines += ["", "## Scenario Results", "", "| ID | Runs | Coverage | Minimal | RCA concept | Fallback |", "| --- | ---: | ---: | ---: | ---: | ---: |"]
    for scenario_id in sorted(by_scenario):
        group = by_scenario[scenario_id]
        lines.append(
            f"| {scenario_id} | {len(group)} | "
            f"{percentage(sum(bool(r.get('planner_pass')) for r in group), len(group)):.1f}% | "
            f"{percentage(sum(set(r.get('required_tools', [])) == set(r.get('selected_tools', [])) for r in group), len(group)):.1f}% | "
            f"{percentage(sum(bool(r.get('rca_pass')) for r in group), len(group)):.1f}% | "
            f"{percentage(sum(bool(r.get('fallback')) for r in group), len(group)):.1f}% |"
        )

lines += [
    "",
    "## Interpretation",
    "",
    "RCA concept accuracy is an automated keyword-based preliminary score.",
    "Review root_cause and fill manual_rca_score in the CSV with 0, 1, or 2 before reporting final accuracy.",
]
with open(summary_path, "w", encoding="utf-8") as handle:
    handle.write("\n".join(lines) + "\n")
print("\n".join(lines))
PY
}

preflight
if [[ "$PREFLIGHT_ONLY" == true ]]; then
  exit 0
fi

scenario_count=15
[[ -z "$SCENARIO" ]] || scenario_count=1
planned_calls=$((REPEAT * scenario_count * 2))
[[ "$WARMUP" == false ]] || planned_calls=$((planned_calls + 2))
log "Starting $((REPEAT * scenario_count)) measured cases; expected Bedrock calls: $planned_calls"

mkdir -p "$(dirname "$OUTPUT")"
args=(
  -fixtures "$FIXTURES"
  -repeat "$REPEAT"
  -cooldown "${COOLDOWN_SECONDS}s"
  -timeout "${TIMEOUT_SECONDS}s"
)
[[ "$WARMUP" == false ]] || args+=(-warmup)
[[ -z "$SCENARIO" ]] || args+=(-scenario "$SCENARIO")

go run ./cmd/rca-benchmark "${args[@]}" > "$OUTPUT"
write_reports
log "JSONL: $OUTPUT"
log "CSV: $CSV"
log "Summary: $SUMMARY"
