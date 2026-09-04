package agent

import "time"

const (
	ToolServiceMetrics   = "service_metrics"
	ToolErrorLogs        = "error_logs"
	ToolWorkloadStatus   = "workload_status"
	ToolKubernetesEvents = "kubernetes_events"
)

var allowedTools = map[string]struct{}{
	ToolServiceMetrics:   {},
	ToolErrorLogs:        {},
	ToolWorkloadStatus:   {},
	ToolKubernetesEvents: {},
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
	Pod       string    `json:"pod"`
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
	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Object  string `json:"object"`
	Message string `json:"message"`
}

type Evidence struct {
	Metrics        *MetricSnapshot   `json:"metrics,omitempty"`
	Logs           []LogSample       `json:"logs,omitempty"`
	Workload       *WorkloadStatus   `json:"workload,omitempty"`
	Events         []KubernetesEvent `json:"kubernetes_events,omitempty"`
	CollectionErrs []string          `json:"collection_errors,omitempty"`
}

type CollectionPlan struct {
	Tools  []string `json:"tools"`
	Reason string   `json:"reason,omitempty"`
}

type RCAResult struct {
	RootCause        string   `json:"root_cause"`
	Confidence       string   `json:"confidence"`
	Evidence         []string `json:"evidence"`
	SuggestedActions []string `json:"suggested_actions"`
}

type Outcome struct {
	Incident Incident   `json:"incident"`
	Path     string     `json:"path"`
	RCA      *RCAResult `json:"rca,omitempty"`
	Fallback bool       `json:"fallback"`
	Error    string     `json:"error,omitempty"`
}

type TokenUsage struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}
