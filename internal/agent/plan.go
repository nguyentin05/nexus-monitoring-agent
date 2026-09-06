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
	if step.Tool == ToolNodeStatus {
		if target != TargetIncidentNode {
			return CollectionStep{}, fmt.Errorf("tool %s requires target %s", step.Tool, TargetIncidentNode)
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
