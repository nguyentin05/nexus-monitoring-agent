package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type LLM interface {
	Plan(context.Context, Incident) (CollectionPlan, TokenUsage, error)
	Analyze(context.Context, Incident) (RCAResult, TokenUsage, error)
}

type EvidenceCollector interface {
	Collect(context.Context, Incident, CollectionPlan) Evidence
}

type OutcomeNotifier interface {
	Send(context.Context, Outcome) error
}

type Stats struct {
	Polls            atomic.Int64
	Received         atomic.Int64
	Deduplicated     atomic.Int64
	Processed        atomic.Int64
	Suppressed       atomic.Int64
	NewPatterns      atomic.Int64
	ExactPattern     atomic.Int64
	KnownPlan        atomic.Int64
	LLMPlanner       atomic.Int64
	Fallback         atomic.Int64
	RCACacheHits     atomic.Int64
	BudgetLimited    atomic.Int64
	PlannerCalls     atomic.Int64
	RCACalls         atomic.Int64
	InputTokens      atomic.Int64
	OutputTokens     atomic.Int64
	CacheReadTokens  atomic.Int64
	CacheWriteTokens atomic.Int64
}

type Processor struct {
	cfg       Config
	collector EvidenceCollector
	llm       LLM
	notifier  OutcomeNotifier
	queue     chan Incident
	seen      map[string]time.Time
	seenMu    sync.Mutex
	budget    *callBudget
	rcaCache  map[string]rcaCacheEntry
	cacheMu   sync.Mutex
	Stats     Stats
}

func NewProcessor(cfg Config, collector EvidenceCollector, llm LLM, notifier OutcomeNotifier) *Processor {
	if cfg.Mode == "" {
		cfg.Mode = "detect"
	}
	if cfg.QueueSize < 1 {
		cfg.QueueSize = 1
	}
	return &Processor{
		cfg:       cfg,
		collector: collector,
		llm:       llm,
		notifier:  notifier,
		queue:     make(chan Incident, cfg.QueueSize),
		seen:      make(map[string]time.Time),
		budget:    newCallBudget(cfg.MaxBedrockCalls),
		rcaCache:  make(map[string]rcaCacheEntry),
	}
}

func (p *Processor) Submit(incident Incident) bool {
	now := time.Now()
	key := incident.Key()
	p.seenMu.Lock()
	for seenKey, expiresAt := range p.seen {
		if now.After(expiresAt) {
			delete(p.seen, seenKey)
		}
	}
	if expiresAt, duplicate := p.seen[key]; duplicate && now.Before(expiresAt) {
		p.seenMu.Unlock()
		p.Stats.Deduplicated.Add(1)
		return true
	}
	p.seen[key] = now.Add(p.cfg.Cooldown)
	p.seenMu.Unlock()

	select {
	case p.queue <- incident:
		p.Stats.Received.Add(1)
		return true
	default:
		p.seenMu.Lock()
		delete(p.seen, key)
		p.seenMu.Unlock()
		return false
	}
}

func (p *Processor) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case incident := <-p.queue:
			outcome := p.Process(ctx, incident)
			slog.Info("incident processed", "incident", incident.Key(), "mode", p.cfg.Mode, "path", outcome.Path, "fallback", outcome.Fallback)
			if p.cfg.Mode != "detect" {
				p.Stats.Suppressed.Add(1)
				continue
			}
			if err := p.notifier.Send(ctx, outcome); err != nil {
				slog.Error("send incident notification", "incident", incident.Key(), "error", err)
			}
		}
	}
}

func (p *Processor) Process(ctx context.Context, incident Incident) Outcome {
	p.Stats.Processed.Add(1)
	if p.cfg.Mode == "training" {
		result := deterministicRCA(incident, "training mode")
		return Outcome{Incident: incident, Path: "training", RCA: &result}
	}

	path := "llm_planner"
	p.Stats.LLMPlanner.Add(1)
	var plannerErr error
	var plan CollectionPlan
	if !p.budget.Allow(time.Now()) {
		p.Stats.BudgetLimited.Add(1)
		plannerErr = fmt.Errorf("LLM hourly call budget exhausted")
		plan = defaultPlan()
	} else {
		p.Stats.PlannerCalls.Add(1)
		var usage TokenUsage
		plan, usage, plannerErr = p.llm.Plan(ctx, incident)
		p.addUsage(usage)
		if plannerErr != nil {
			plan = defaultPlan()
		}
	}

	incident.Evidence = p.collector.Collect(ctx, incident, plan)
	if !p.budget.Allow(time.Now()) {
		p.Stats.BudgetLimited.Add(1)
		return p.fallback(incident, path, plannerErr, fmt.Errorf("LLM hourly call budget exhausted"))
	}

	p.Stats.RCACalls.Add(1)
	result, usage, err := p.llm.Analyze(ctx, incident)
	p.addUsage(usage)
	if err != nil {
		return p.fallback(incident, path, plannerErr, err)
	}
	return Outcome{Incident: incident, Path: path, RCA: &result}
}

func (p *Processor) fallback(incident Incident, path string, plannerErr, rcaErr error) Outcome {
	p.Stats.Fallback.Add(1)
	message := rcaErr.Error()
	if plannerErr != nil {
		message = fmt.Sprintf("planner: %v; RCA: %v", plannerErr, rcaErr)
	}
	result := deterministicRCA(incident, message)
	return Outcome{Incident: incident, Path: path, RCA: &result, Fallback: true, Error: message}
}

func (p *Processor) cachedRCA(key string) (rcaCacheEntry, bool) {
	if p.cfg.RCACacheTTL <= 0 {
		return rcaCacheEntry{}, false
	}
	now := time.Now()
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	for cacheKey, entry := range p.rcaCache {
		if now.After(entry.ExpiresAt) {
			delete(p.rcaCache, cacheKey)
		}
	}
	entry, ok := p.rcaCache[key]
	return entry, ok
}

func (p *Processor) cacheRCA(key, path string, result RCAResult) {
	if p.cfg.RCACacheTTL <= 0 {
		return
	}
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	if len(p.rcaCache) >= 512 {
		var oldestKey string
		var oldest time.Time
		for cacheKey, entry := range p.rcaCache {
			if oldestKey == "" || entry.ExpiresAt.Before(oldest) {
				oldestKey, oldest = cacheKey, entry.ExpiresAt
			}
		}
		delete(p.rcaCache, oldestKey)
	}
	p.rcaCache[key] = rcaCacheEntry{Result: result, Path: path, ExpiresAt: time.Now().Add(p.cfg.RCACacheTTL)}
}

func (p *Processor) addUsage(usage TokenUsage) {
	p.Stats.InputTokens.Add(usage.Input)
	p.Stats.OutputTokens.Add(usage.Output)
	p.Stats.CacheReadTokens.Add(usage.CacheRead)
	p.Stats.CacheWriteTokens.Add(usage.CacheWrite)
}

func knownPlan(incident Incident) (CollectionPlan, bool) {
	plans := map[string][]string{
		"error_rate_high":   {ToolServiceMetrics, ToolErrorLogs, ToolWorkloadStatus},
		"latency_high":      {ToolServiceMetrics, ToolErrorLogs},
		"cpu_high":          {ToolServiceMetrics, ToolWorkloadStatus, ToolErrorLogs},
		"frequent_restarts": {ToolServiceMetrics, ToolErrorLogs, ToolWorkloadStatus, ToolKubernetesEvents},
		"exception_pattern": {ToolServiceMetrics, ToolErrorLogs, ToolWorkloadStatus},
		"novel_log_pattern": {ToolServiceMetrics, ToolErrorLogs, ToolWorkloadStatus},
	}
	tools, ok := plans[incident.Kind]
	if !ok {
		return CollectionPlan{}, false
	}
	return CollectionPlan{Tools: tools, Reason: "predefined plan for " + incident.Kind}, true
}

func defaultPlan() CollectionPlan {
	return CollectionPlan{Tools: []string{ToolServiceMetrics, ToolErrorLogs, ToolWorkloadStatus, ToolKubernetesEvents}, Reason: "safe fallback plan"}
}

func deterministicRCA(incident Incident, reason string) RCAResult {
	evidence := []string{incident.Description}
	if len(incident.Evidence.CollectionErrs) > 0 {
		evidence = append(evidence, "Some evidence collectors failed: "+strings.Join(incident.Evidence.CollectionErrs, "; "))
	}
	actions := []string{"Inspect the collected metrics, logs, workload status and Kubernetes events.", "Correlate the incident timestamp with the latest deployment or configuration change."}
	switch incident.Kind {
	case "cpu_high":
		actions[0] = "Inspect per-Pod CPU usage, request rate and CPU throttling before changing resource limits."
	case "error_rate_high", "novel_log_pattern":
		actions[0] = "Inspect the newest error logs and compare them with the last healthy deployment."
	case "frequent_restarts":
		actions[0] = "Inspect container termination reasons, probes, limits and recent Kubernetes events."
	}
	return RCAResult{
		RootCause:        "The signal was detected, but AI root-cause analysis was unavailable: " + reason,
		Confidence:       "low",
		Evidence:         evidence,
		SuggestedActions: actions,
	}
}

type pattern struct {
	all    []string
	result RCAResult
}

var exactPatterns = []pattern{
	{all: []string{"imagepullbackoff", "404"}, result: RCAResult{RootCause: "The configured container image tag does not exist in the registry.", Confidence: "high", SuggestedActions: []string{"Verify the GitOps image tag and confirm that the image exists in ECR."}}},
	{all: []string{"imagepullbackoff", "401"}, result: RCAResult{RootCause: "The workload cannot authenticate to the container registry.", Confidence: "high", SuggestedActions: []string{"Verify the node ECR permissions and registry configuration."}}},
	{all: []string{"no signatures found"}, result: RCAResult{RootCause: "Kyverno rejected an unsigned container image.", Confidence: "high", SuggestedActions: []string{"Publish and sign the image through the release pipeline before updating GitOps."}}},
	{all: []string{"oomkilled"}, result: RCAResult{RootCause: "The container exceeded its memory limit and was terminated by the kernel.", Confidence: "high", SuggestedActions: []string{"Inspect memory usage and leaks before adjusting the container memory limit."}}},
	{all: []string{"untolerated taint"}, result: RCAResult{RootCause: "The Pod cannot be scheduled because it does not tolerate the available node taints.", Confidence: "high", SuggestedActions: []string{"Align the Pod tolerations and node placement policy."}}},
}

func exactPattern(incident Incident) (RCAResult, bool) {
	raw, _ := json.Marshal(struct {
		Description string   `json:"description"`
		Evidence    Evidence `json:"evidence"`
	}{incident.Description, incident.Evidence})
	text := strings.ToLower(string(raw))
	for _, candidate := range exactPatterns {
		matched := true
		for _, fragment := range candidate.all {
			if !strings.Contains(text, fragment) {
				matched = false
				break
			}
		}
		if matched {
			return candidate.result, true
		}
	}
	return RCAResult{}, false
}

func (p *Processor) Metrics() string {
	lines := []string{
		"# TYPE nexus_agent_polls_total counter", fmt.Sprintf("nexus_agent_polls_total %d", p.Stats.Polls.Load()),
		"# TYPE nexus_agent_incidents_received_total counter", fmt.Sprintf("nexus_agent_incidents_received_total %d", p.Stats.Received.Load()),
		"# TYPE nexus_agent_incidents_deduplicated_total counter", fmt.Sprintf("nexus_agent_incidents_deduplicated_total %d", p.Stats.Deduplicated.Load()),
		"# TYPE nexus_agent_incidents_processed_total counter", fmt.Sprintf("nexus_agent_incidents_processed_total %d", p.Stats.Processed.Load()),
		"# TYPE nexus_agent_incidents_suppressed_total counter", fmt.Sprintf("nexus_agent_incidents_suppressed_total %d", p.Stats.Suppressed.Load()),
		"# TYPE nexus_agent_new_patterns_total counter", fmt.Sprintf("nexus_agent_new_patterns_total %d", p.Stats.NewPatterns.Load()),
		"# TYPE nexus_agent_path_exact_pattern_total counter", fmt.Sprintf("nexus_agent_path_exact_pattern_total %d", p.Stats.ExactPattern.Load()),
		"# TYPE nexus_agent_path_known_plan_total counter", fmt.Sprintf("nexus_agent_path_known_plan_total %d", p.Stats.KnownPlan.Load()),
		"# TYPE nexus_agent_path_llm_planner_total counter", fmt.Sprintf("nexus_agent_path_llm_planner_total %d", p.Stats.LLMPlanner.Load()),
		"# TYPE nexus_agent_fallback_total counter", fmt.Sprintf("nexus_agent_fallback_total %d", p.Stats.Fallback.Load()),
		"# TYPE nexus_agent_rca_cache_hits_total counter", fmt.Sprintf("nexus_agent_rca_cache_hits_total %d", p.Stats.RCACacheHits.Load()),
		"# TYPE nexus_agent_bedrock_budget_limited_total counter", fmt.Sprintf("nexus_agent_bedrock_budget_limited_total %d", p.Stats.BudgetLimited.Load()),
		"# TYPE nexus_agent_bedrock_calls_total counter", fmt.Sprintf("nexus_agent_bedrock_calls_total{stage=\"planner\"} %d", p.Stats.PlannerCalls.Load()), fmt.Sprintf("nexus_agent_bedrock_calls_total{stage=\"rca\"} %d", p.Stats.RCACalls.Load()),
		"# TYPE nexus_agent_bedrock_tokens_total counter", fmt.Sprintf("nexus_agent_bedrock_tokens_total{type=\"input\"} %d", p.Stats.InputTokens.Load()), fmt.Sprintf("nexus_agent_bedrock_tokens_total{type=\"output\"} %d", p.Stats.OutputTokens.Load()), fmt.Sprintf("nexus_agent_bedrock_tokens_total{type=\"cache_read\"} %d", p.Stats.CacheReadTokens.Load()), fmt.Sprintf("nexus_agent_bedrock_tokens_total{type=\"cache_write\"} %d", p.Stats.CacheWriteTokens.Load()),
		"# TYPE nexus_agent_queue_depth gauge", fmt.Sprintf("nexus_agent_queue_depth %d", len(p.queue)),
	}
	return strings.Join(lines, "\n") + "\n"
}
