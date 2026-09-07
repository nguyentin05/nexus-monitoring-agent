package agent

import "time"

const (
	ToolServiceMetrics   = "service_metrics"
	ToolErrorLogs        = "error_logs"
	ToolWorkloadStatus   = "workload_status"
	ToolKubernetesEvents = "kubernetes_events"
	ToolNetworkPolicies  = "network_policies"
	ToolTraceContext     = "trace_context"
	ToolRecentChanges    = "recent_changes"
	ToolNodeStatus       = "node_status"

	TargetIncidentService = "incident.service"
	TargetIncidentPods    = "incident.pods"
	TargetIncidentNode    = "incident.node"
	TargetRelatedNode     = "related.node"
	TargetRelatedOperator = "related.operator"
)

var allowedTools = map[string]struct{}{
	ToolServiceMetrics:   {},
	ToolErrorLogs:        {},
	ToolWorkloadStatus:   {},
	ToolKubernetesEvents: {},
	ToolNetworkPolicies:  {},
	ToolTraceContext:     {},
	ToolRecentChanges:    {},
	ToolNodeStatus:       {},
}

type Incident struct {
	Source         string    `json:"source"`
	AlertName      string    `json:"alert_name"`
	Kind           string    `json:"kind"`
	Service        string    `json:"service"`
	Namespace      string    `json:"namespace"`
	Severity       string    `json:"severity"`
	Description    string    `json:"description"`
	Fingerprint    string    `json:"fingerprint,omitempty"`
	CorrelationKey string    `json:"correlation_key,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	Evidence       Evidence  `json:"evidence,omitempty"`
}

func (i Incident) Key() string {
	if i.CorrelationKey != "" {
		return i.Namespace + ":" + i.CorrelationKey
	}
	key := i.Namespace + ":" + i.Service + ":" + i.Kind
	if i.Fingerprint != "" {
		key += ":" + i.Fingerprint
	}
	return key
}

type MetricSnapshot struct {
	CPUPercent       *float64 `json:"cpu_percent,omitempty"`
	MemoryMB         *float64 `json:"memory_mb,omitempty"`
	ErrorRatePercent *float64 `json:"error_rate_percent,omitempty"`
	P99LatencyMS     *float64 `json:"p99_latency_ms,omitempty"`
	RestartCount     *float64 `json:"restart_count_15m,omitempty"`
}

type LogSample struct {
	Timestamp time.Time `json:"timestamp"`
	Namespace string    `json:"namespace,omitempty"`
	Pod       string    `json:"pod"`
	Container string    `json:"container,omitempty"`
	Message   string    `json:"message"`
}

type WorkloadStatus struct {
	DesiredReplicas    int32    `json:"desired_replicas"`
	AvailableReplicas  int32    `json:"available_replicas"`
	ReadyPods          int      `json:"ready_pods"`
	TotalPods          int      `json:"total_pods"`
	Restarts           int32    `json:"restarts"`
	TerminationReasons []string `json:"termination_reasons,omitempty"`
}

type KubernetesEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Reason    string    `json:"reason"`
	Object    string    `json:"object"`
	Message   string    `json:"message"`
}

type NetworkPolicyEvidence struct {
	Name            string            `json:"name"`
	PolicyTypes     []string          `json:"policy_types"`
	PodSelector     map[string]string `json:"pod_selector"`
	EgressRuleCount int               `json:"egress_rule_count"`
	AllowedPorts    []string          `json:"allowed_ports,omitempty"`
}

type TraceEvidence struct {
	TraceID     string    `json:"trace_id"`
	RootService string    `json:"root_service,omitempty"`
	RootName    string    `json:"root_name,omitempty"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	DurationMS  float64   `json:"duration_ms"`
}

type DeploymentChange struct {
	ReplicaSet string    `json:"replica_set"`
	Revision   string    `json:"revision,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	Images     []string  `json:"images,omitempty"`
	Replicas   int32     `json:"replicas"`
	Ready      int32     `json:"ready"`
}

type NodeStatusEvidence struct {
	Name        string            `json:"name"`
	Conditions  map[string]string `json:"conditions,omitempty"`
	Capacity    map[string]string `json:"capacity,omitempty"`
	Allocatable map[string]string `json:"allocatable,omitempty"`
	Taints      []string          `json:"taints,omitempty"`
}

type Evidence struct {
	Metrics         *MetricSnapshot         `json:"metrics,omitempty"`
	Logs            []LogSample             `json:"logs,omitempty"`
	Workload        *WorkloadStatus         `json:"workload,omitempty"`
	Events          []KubernetesEvent       `json:"kubernetes_events,omitempty"`
	NetworkPolicies []NetworkPolicyEvidence `json:"network_policies,omitempty"`
	Traces          []TraceEvidence         `json:"traces,omitempty"`
	RecentChanges   []DeploymentChange      `json:"recent_changes,omitempty"`
	NodeStatus      *NodeStatusEvidence     `json:"node_status,omitempty"`
	CollectionErrs  []string                `json:"collection_errors,omitempty"`
	EvidenceGaps    []string                `json:"evidence_gaps,omitempty"`
}

type CollectionStep struct {
	Tool            string   `json:"tool"`
	Target          string   `json:"target,omitempty"`
	LookbackMinutes int      `json:"lookback_minutes,omitempty"`
	Limit           int      `json:"limit,omitempty"`
	Metrics         []string `json:"metrics,omitempty"`
	LogTerms        []string `json:"log_terms,omitempty"`
	TraceStatus     string   `json:"trace_status,omitempty"`
	MinDurationMS   int64    `json:"min_duration_ms,omitempty"`
}

type CollectionPlan struct {
	Steps  []CollectionStep `json:"steps"`
	Reason string           `json:"reason,omitempty"`
}

type RCAResult struct {
	RootCause        string   `json:"root_cause"`
	Confidence       string   `json:"confidence"`
	Evidence         []string `json:"evidence"`
	SuggestedActions []string `json:"suggested_actions"`
}

type Outcome struct {
	Incident         Incident   `json:"incident"`
	Path             string     `json:"path"`
	RCA              *RCAResult `json:"rca,omitempty"`
	Fallback         bool       `json:"fallback"`
	Grounded         bool       `json:"grounded"`
	EvidenceComplete bool       `json:"evidence_complete"`
	Error            string     `json:"error,omitempty"`
}

type TokenUsage struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}
