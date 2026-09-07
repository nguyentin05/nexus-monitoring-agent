package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

const (
	MetricCPU       = "cpu"
	MetricMemory    = "memory"
	MetricErrorRate = "error_rate"
	MetricP99       = "p99_latency"
	MetricRestarts  = "restarts"

	TraceStatusError = "error"
	TraceStatusSlow  = "slow"
	TraceStatusAll   = "all"

	maxCollectionSteps = 8
	maxLookbackMinutes = 30
	maxCollectionLimit = 20
)

const (
	familyApplication  = "application"
	familyCompute      = "compute"
	familyDependency   = "dependency"
	familyDeployment   = "deployment"
	familyGeneric      = "generic"
	familyLatency      = "latency"
	familyNetwork      = "network"
	familyNode         = "node"
	familyPodLifecycle = "pod_lifecycle"
	familySecrets      = "secrets"
	familyStorage      = "storage"
)

type evidenceContract struct {
	Family   string
	Required []CollectionStep
}

var allowedMetrics = map[string]struct{}{
	MetricCPU: {}, MetricMemory: {}, MetricErrorRate: {}, MetricP99: {}, MetricRestarts: {},
}

func normalizeCollectionPlan(plan CollectionPlan) (CollectionPlan, error) {
	if len(plan.Steps) == 0 {
		return CollectionPlan{}, fmt.Errorf("collection plan has no steps")
	}
	if len(plan.Steps) > maxCollectionSteps {
		return CollectionPlan{}, fmt.Errorf("collection plan exceeds %d steps", maxCollectionSteps)
	}

	normalized := CollectionPlan{Reason: strings.TrimSpace(plan.Reason)}
	if len(normalized.Reason) > 300 {
		normalized.Reason = normalized.Reason[:300]
	}
	seen := make(map[string]struct{}, len(plan.Steps))
	for _, step := range plan.Steps {
		next, err := normalizeCollectionStep(step)
		if err != nil {
			return CollectionPlan{}, err
		}
		key := collectionStepKey(next)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		normalized.Steps = append(normalized.Steps, next)
	}
	sort.Slice(normalized.Steps, func(i, j int) bool {
		return collectionStepKey(normalized.Steps[i]) < collectionStepKey(normalized.Steps[j])
	})
	return normalized, nil
}

func normalizeCollectionStep(step CollectionStep) (CollectionStep, error) {
	if step.LookbackMinutes < 0 || step.LookbackMinutes > maxLookbackMinutes {
		return CollectionStep{}, fmt.Errorf("lookback_minutes must be between 0 and %d", maxLookbackMinutes)
	}
	if step.Limit < 0 || step.Limit > maxCollectionLimit {
		return CollectionStep{}, fmt.Errorf("limit must be between 0 and %d", maxCollectionLimit)
	}
	if _, ok := allowedTools[step.Tool]; !ok {
		return CollectionStep{}, fmt.Errorf("unsupported collection tool %q", step.Tool)
	}
	target := step.Target
	if target == "" {
		target = TargetIncidentService
		if step.Tool == ToolNodeStatus {
			target = TargetIncidentNode
		}
	}
	allowedTargets := map[string]map[string]struct{}{
		ToolErrorLogs:        {TargetIncidentService: {}, TargetRelatedOperator: {}},
		ToolWorkloadStatus:   {TargetIncidentService: {}, TargetIncidentPods: {}},
		ToolKubernetesEvents: {TargetIncidentService: {}, TargetIncidentPods: {}},
		ToolNodeStatus:       {TargetIncidentNode: {}, TargetRelatedNode: {}},
	}
	if targets, restricted := allowedTargets[step.Tool]; restricted {
		if _, ok := targets[target]; !ok {
			return CollectionStep{}, fmt.Errorf("tool %s does not support target %s", step.Tool, target)
		}
	} else if target != TargetIncidentService {
		return CollectionStep{}, fmt.Errorf("tool %s requires target %s", step.Tool, TargetIncidentService)
	}
	next := CollectionStep{Tool: step.Tool, Target: target}
	switch step.Tool {
	case ToolServiceMetrics:
		metrics, err := normalizeEnumValues(step.Metrics, allowedMetrics, 5, "metric")
		if err != nil {
			return CollectionStep{}, err
		}
		if len(metrics) == 0 {
			metrics = []string{MetricCPU, MetricErrorRate, MetricMemory, MetricP99, MetricRestarts}
		}
		next.Metrics = metrics
	case ToolErrorLogs:
		next.LookbackMinutes = boundedValue(step.LookbackMinutes, 5, maxLookbackMinutes)
		next.Limit = boundedValue(step.Limit, 10, maxCollectionLimit)
		terms, err := normalizeLogTerms(step.LogTerms)
		if err != nil {
			return CollectionStep{}, err
		}
		next.LogTerms = terms
	case ToolKubernetesEvents:
		next.LookbackMinutes = boundedValue(step.LookbackMinutes, 10, maxLookbackMinutes)
		next.Limit = boundedValue(step.Limit, 10, maxCollectionLimit)
	case ToolTraceContext:
		if step.Limit > 5 {
			return CollectionStep{}, fmt.Errorf("trace limit exceeds 5")
		}
		next.LookbackMinutes = boundedValue(step.LookbackMinutes, 10, maxLookbackMinutes)
		next.Limit = boundedValue(step.Limit, 3, 5)
		next.TraceStatus = step.TraceStatus
		if next.TraceStatus == "" {
			next.TraceStatus = TraceStatusError
		}
		if next.TraceStatus != TraceStatusError && next.TraceStatus != TraceStatusSlow && next.TraceStatus != TraceStatusAll {
			return CollectionStep{}, fmt.Errorf("unsupported trace status %q", next.TraceStatus)
		}
		if next.TraceStatus == TraceStatusSlow {
			next.MinDurationMS = step.MinDurationMS
			if next.MinDurationMS <= 0 {
				next.MinDurationMS = 500
			}
			if next.MinDurationMS > 60_000 {
				return CollectionStep{}, fmt.Errorf("trace minimum duration exceeds 60000ms")
			}
		}
	case ToolRecentChanges:
		if step.Limit > 10 {
			return CollectionStep{}, fmt.Errorf("recent changes limit exceeds 10")
		}
		next.LookbackMinutes = boundedValue(step.LookbackMinutes, 30, maxLookbackMinutes)
		next.Limit = boundedValue(step.Limit, 5, 10)
	}
	return next, nil
}

func normalizeEnumValues(values []string, allowed map[string]struct{}, max int, name string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if _, ok := allowed[value]; !ok {
			return nil, fmt.Errorf("unsupported %s %q", name, value)
		}
		seen[value] = struct{}{}
	}
	if len(seen) > max {
		return nil, fmt.Errorf("too many %s values", name)
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func normalizeLogTerms(values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if len(value) < 3 || len(value) > 64 || strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("invalid log term %q", value)
		}
		seen[value] = struct{}{}
	}
	if len(seen) > 5 {
		return nil, fmt.Errorf("too many log terms")
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func boundedValue(value, fallback, maximum int) int {
	if value == 0 {
		return fallback
	}
	if value < 0 || value > maximum {
		return maximum + 1
	}
	return value
}

func collectionStepKey(step CollectionStep) string {
	encoded, _ := json.Marshal(step)
	return string(encoded)
}

func collectionPlanFingerprint(plan CollectionPlan) string {
	normalized, err := normalizeCollectionPlan(plan)
	if err != nil {
		return ""
	}
	encoded, _ := json.Marshal(normalized.Steps)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:12])
}

func collectionPlansEqual(left, right CollectionPlan) bool {
	leftFingerprint := collectionPlanFingerprint(left)
	return leftFingerprint != "" && leftFingerprint == collectionPlanFingerprint(right)
}

func cloneCollectionPlan(plan CollectionPlan) CollectionPlan {
	clone := CollectionPlan{Reason: plan.Reason, Steps: make([]CollectionStep, len(plan.Steps))}
	for index, step := range plan.Steps {
		clone.Steps[index] = step
		clone.Steps[index].Metrics = slices.Clone(step.Metrics)
		clone.Steps[index].LogTerms = slices.Clone(step.LogTerms)
	}
	return clone
}

func incidentFamily(incident Incident) string {
	text := strings.ToLower(strings.Join([]string{incident.Kind, incident.AlertName, incident.Description}, " "))
	text = strings.ReplaceAll(text, "without cpu saturation", "")
	switch {
	case containsAny(text, "vault", "external secret", "secret reconciliation", "required credentials"):
		return familySecrets
	case containsAny(text, "networkpolicy", "network policy", "policy change"):
		return familyNetwork
	case containsAny(text, "diskpressure", "disk pressure", "ephemeral storage", "ephemeral-storage"):
		return familyStorage
	case containsAny(text, "image pull", "before its container starts", "resourcequota", "resource quota", "cannot create", "failedcreate"):
		return familyDeployment
	case containsAny(text, "oom", "memory growth", "liveness", "probe", "frequent_restarts", "repeatedly restarts"):
		return familyPodLifecycle
	case containsAny(text, "node_cpu", "node_memory"):
		return familyNode
	case containsAny(text, "cpu_high", "cpu usage", "cpu saturation"):
		return familyCompute
	case containsAny(text, "p99", "latency", "slow response"):
		return familyLatency
	case containsAny(text, "5xx", "error_rate_high", "exception_pattern", "novel_log_pattern"):
		return familyApplication
	case containsAny(text, "dependency", "database", "persistence", "dns", "certificate", "tls", "upstream"):
		return familyDependency
	default:
		return familyGeneric
	}
}

func contractForIncident(incident Incident) evidenceContract {
	contract := evidenceContract{Family: incidentFamily(incident)}
	switch contract.Family {
	case familyApplication:
		contract.Required = []CollectionStep{
			{Tool: ToolServiceMetrics, Metrics: []string{MetricErrorRate}},
			{Tool: ToolErrorLogs},
		}
	case familyCompute:
		contract.Required = []CollectionStep{
			{Tool: ToolServiceMetrics, Metrics: []string{MetricCPU, MetricMemory}},
			{Tool: ToolWorkloadStatus, Target: TargetIncidentPods},
		}
	case familyLatency:
		contract.Required = []CollectionStep{
			{Tool: ToolServiceMetrics, Metrics: []string{MetricCPU, MetricP99}},
			{Tool: ToolErrorLogs, LogTerms: []string{"latency", "query", "slow", "warning"}},
		}
	case familyDependency:
		contract.Required = []CollectionStep{
			{Tool: ToolErrorLogs, LogTerms: []string{"certificate", "dns", "error", "refused", "timeout"}},
		}
	case familyPodLifecycle:
		contract.Required = []CollectionStep{
			{Tool: ToolWorkloadStatus, Target: TargetIncidentPods},
			{Tool: ToolKubernetesEvents, Target: TargetIncidentPods},
		}
	case familyDeployment:
		contract.Required = []CollectionStep{
			{Tool: ToolWorkloadStatus, Target: TargetIncidentPods},
			{Tool: ToolKubernetesEvents, Target: TargetIncidentPods},
		}
	case familyStorage:
		contract.Required = []CollectionStep{
			{Tool: ToolKubernetesEvents, Target: TargetIncidentPods},
			{Tool: ToolNodeStatus, Target: TargetRelatedNode},
		}
	case familyNetwork:
		contract.Required = []CollectionStep{
			{Tool: ToolErrorLogs, LogTerms: []string{"blocked", "deny", "error", "timeout", "unreachable"}},
			{Tool: ToolNetworkPolicies},
		}
	case familySecrets:
		contract.Required = []CollectionStep{
			{Tool: ToolKubernetesEvents, Target: TargetIncidentPods},
			{Tool: ToolErrorLogs, Target: TargetRelatedOperator, LogTerms: []string{"error", "permission", "reconcile", "secret", "vault"}},
		}
	case familyNode:
		contract.Required = []CollectionStep{{Tool: ToolNodeStatus, Target: TargetIncidentNode}}
	default:
		contract.Required = []CollectionStep{
			{Tool: ToolErrorLogs},
			{Tool: ToolWorkloadStatus, Target: TargetIncidentPods},
		}
	}
	return contract
}

func mergeContract(plan CollectionPlan, contract evidenceContract) CollectionPlan {
	steps := append([]CollectionStep(nil), contract.Required...)
	steps = append(steps, plan.Steps...)
	merged := CollectionPlan{Reason: plan.Reason}
	seen := make(map[string]struct{}, maxCollectionSteps)
	for _, step := range steps {
		normalized, err := normalizeCollectionStep(step)
		if err != nil {
			continue
		}
		if !contract.allows(normalized.Tool) {
			continue
		}
		key := normalized.Tool + "\x00" + normalized.Target
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		merged.Steps = append(merged.Steps, normalized)
		if len(merged.Steps) == maxCollectionSteps {
			break
		}
	}
	sort.Slice(merged.Steps, func(i, j int) bool {
		return collectionStepKey(merged.Steps[i]) < collectionStepKey(merged.Steps[j])
	})
	return merged
}

func (contract evidenceContract) allows(tool string) bool {
	switch tool {
	case ToolNodeStatus:
		return contract.Family == familyNode || contract.Family == familyStorage
	case ToolNetworkPolicies:
		return contract.Family == familyNetwork || contract.Family == familyDependency
	case ToolKubernetesEvents:
		return contract.Family == familyPodLifecycle || contract.Family == familyDeployment || contract.Family == familyStorage || contract.Family == familySecrets || contract.Family == familyNetwork
	default:
		return true
	}
}

func (contract evidenceContract) gaps(evidence Evidence) []string {
	var gaps []string
	for _, raw := range contract.Required {
		step, err := normalizeCollectionStep(raw)
		if err != nil {
			continue
		}
		if !evidenceAvailable(evidence, step) {
			gaps = append(gaps, step.Tool+"@"+step.Target)
		}
	}
	return gaps
}

func evidenceAvailable(evidence Evidence, step CollectionStep) bool {
	switch step.Tool {
	case ToolServiceMetrics:
		if evidence.Metrics == nil {
			return false
		}
		for _, metric := range step.Metrics {
			value := map[string]*float64{
				MetricCPU: evidence.Metrics.CPUPercent, MetricMemory: evidence.Metrics.MemoryMB,
				MetricErrorRate: evidence.Metrics.ErrorRatePercent, MetricP99: evidence.Metrics.P99LatencyMS,
				MetricRestarts: evidence.Metrics.RestartCount,
			}[metric]
			if value == nil {
				return false
			}
		}
		return true
	case ToolErrorLogs:
		if step.Target == TargetRelatedOperator {
			for _, sample := range evidence.Logs {
				if sample.Namespace == "external-secrets" && sample.Container == "external-secrets" {
					return true
				}
			}
			return false
		}
		return len(evidence.Logs) > 0
	case ToolWorkloadStatus:
		return evidence.Workload != nil
	case ToolKubernetesEvents:
		return len(evidence.Events) > 0
	case ToolNetworkPolicies:
		return len(evidence.NetworkPolicies) > 0
	case ToolTraceContext:
		return len(evidence.Traces) > 0
	case ToolRecentChanges:
		return len(evidence.RecentChanges) > 0
	case ToolNodeStatus:
		return evidence.NodeStatus != nil
	default:
		return false
	}
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}
