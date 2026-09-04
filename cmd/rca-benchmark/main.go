package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/nguyentin05/nexus-monitoring-agent/internal/agent"
)

type scenario struct {
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	Category         string         `json:"category"`
	Description      string         `json:"description"`
	GroundTruth      string         `json:"ground_truth"`
	RequiredTools    []string       `json:"required_tools"`
	ExpectedConcepts [][]string     `json:"expected_concepts"`
	Evidence         agent.Evidence `json:"available_evidence"`
}

type fixtureCollector struct {
	available agent.Evidence
	selected  []string
}

func (c *fixtureCollector) Collect(_ context.Context, _ agent.Incident, plan agent.CollectionPlan) agent.Evidence {
	c.selected = append([]string(nil), plan.Tools...)
	var evidence agent.Evidence
	for _, tool := range plan.Tools {
		switch tool {
		case agent.ToolServiceMetrics:
			evidence.Metrics = c.available.Metrics
		case agent.ToolErrorLogs:
			evidence.Logs = c.available.Logs
		case agent.ToolWorkloadStatus:
			evidence.Workload = c.available.Workload
		case agent.ToolKubernetesEvents:
			evidence.Events = c.available.Events
		}
	}
	return evidence
}

type noopNotifier struct{}

func (noopNotifier) Send(context.Context, agent.Outcome) error { return nil }

type result struct {
	Run              int      `json:"run"`
	Warmup           bool     `json:"warmup"`
	ScenarioID       string   `json:"scenario_id"`
	Scenario         string   `json:"scenario"`
	Category         string   `json:"category"`
	GroundTruth      string   `json:"ground_truth"`
	RequiredTools    []string `json:"required_tools"`
	SelectedTools    []string `json:"selected_tools"`
	PlannerPass      bool     `json:"planner_pass"`
	RCAPass          bool     `json:"rca_pass"`
	Path             string   `json:"path"`
	RootCause        string   `json:"root_cause"`
	Confidence       string   `json:"confidence"`
	Evidence         []string `json:"evidence"`
	SuggestedActions []string `json:"suggested_actions"`
	Fallback         bool     `json:"fallback"`
	Error            string   `json:"error,omitempty"`
	LatencyMS        int64    `json:"latency_ms"`
	PlannerCalls     int64    `json:"planner_calls"`
	RCACalls         int64    `json:"rca_calls"`
	InputTokens      int64    `json:"input_tokens"`
	OutputTokens     int64    `json:"output_tokens"`
	CacheReadTokens  int64    `json:"cache_read_tokens"`
	CacheWriteTokens int64    `json:"cache_write_tokens"`
}

func main() {
	var (
		fixtures = flag.String("fixtures", "benchmark/rca-scenarios.json", "scenario fixture file")
		region   = flag.String("region", env("AWS_REGION", "ap-southeast-1"), "AWS region")
		model    = flag.String("model", env("BEDROCK_MODEL_ID", "global.amazon.nova-2-lite-v1:0"), "Bedrock model ID")
		repeat   = flag.Int("repeat", 1, "measured repetitions per scenario")
		warmup   = flag.Bool("warmup", false, "run the first selected scenario once as warm-up")
		cooldown = flag.Duration("cooldown", 5*time.Second, "pause between Bedrock cases")
		timeout  = flag.Duration("timeout", 90*time.Second, "timeout per scenario")
		only     = flag.String("scenario", "", "run only one scenario ID")
	)
	flag.Parse()
	if *repeat < 1 || *cooldown < 0 || *timeout <= 0 {
		fatal("repeat and timeout must be positive; cooldown must be non-negative")
	}

	scenarios, err := loadScenarios(*fixtures, *only)
	if err != nil {
		fatal(err.Error())
	}
	ctx := context.Background()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(*region))
	if err != nil {
		fatal("load AWS config: " + err.Error())
	}
	llm := agent.NewBedrock(bedrockruntime.NewFromConfig(awsCfg), *model, *timeout)
	encoder := json.NewEncoder(os.Stdout)

	if *warmup {
		if err := runCase(ctx, encoder, llm, scenarios[0], 0, true); err != nil {
			fatal(err.Error())
		}
		pause(*cooldown)
	}
	for run := 1; run <= *repeat; run++ {
		for index, item := range scenarios {
			fmt.Fprintf(os.Stderr, "[%s] run=%d scenario=%s\n", time.Now().UTC().Format(time.RFC3339), run, item.ID)
			if err := runCase(ctx, encoder, llm, item, run, false); err != nil {
				fatal(err.Error())
			}
			if run != *repeat || index != len(scenarios)-1 {
				pause(*cooldown)
			}
		}
	}
}

func runCase(parent context.Context, encoder *json.Encoder, llm agent.LLM, item scenario, run int, warmup bool) error {
	collector := &fixtureCollector{available: item.Evidence}
	processor := agent.NewProcessor(agent.Config{
		Mode:            "detect",
		QueueSize:       1,
		MaxBedrockCalls: 0,
		RCACacheTTL:     0,
	}, collector, llm, noopNotifier{})
	incident := agent.Incident{
		Source:      "benchmark",
		AlertName:   "Benchmark" + strings.ReplaceAll(item.ID, "-", ""),
		Kind:        "benchmark_" + strings.ToLower(item.ID),
		Service:     "aiops-benchmark-service",
		Namespace:   "apps",
		Severity:    "critical",
		Description: item.Description,
		Fingerprint: fmt.Sprintf("%s-%d-%d", item.ID, run, time.Now().UnixNano()),
		StartedAt:   time.Now().UTC(),
	}

	started := time.Now()
	outcome := processor.Process(parent, incident)
	record := result{
		Run:              run,
		Warmup:           warmup,
		ScenarioID:       item.ID,
		Scenario:         item.Name,
		Category:         item.Category,
		GroundTruth:      item.GroundTruth,
		RequiredTools:    item.RequiredTools,
		SelectedTools:    collector.selected,
		PlannerPass:      containsAll(collector.selected, item.RequiredTools),
		Path:             outcome.Path,
		Fallback:         outcome.Fallback,
		Error:            outcome.Error,
		LatencyMS:        time.Since(started).Milliseconds(),
		PlannerCalls:     processor.Stats.PlannerCalls.Load(),
		RCACalls:         processor.Stats.RCACalls.Load(),
		InputTokens:      processor.Stats.InputTokens.Load(),
		OutputTokens:     processor.Stats.OutputTokens.Load(),
		CacheReadTokens:  processor.Stats.CacheReadTokens.Load(),
		CacheWriteTokens: processor.Stats.CacheWriteTokens.Load(),
	}
	if outcome.RCA != nil {
		record.RootCause = outcome.RCA.RootCause
		record.Confidence = outcome.RCA.Confidence
		record.Evidence = outcome.RCA.Evidence
		record.SuggestedActions = outcome.RCA.SuggestedActions
		record.RCAPass = matchesConcepts(outcome.RCA.RootCause, item.ExpectedConcepts)
	}
	return encoder.Encode(record)
}

func loadScenarios(path, only string) ([]scenario, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fixtures: %w", err)
	}
	var scenarios []scenario
	if err := json.Unmarshal(raw, &scenarios); err != nil {
		return nil, fmt.Errorf("decode fixtures: %w", err)
	}
	selected := scenarios[:0]
	seen := make(map[string]struct{}, len(scenarios))
	for _, item := range scenarios {
		if item.ID == "" || item.Name == "" || item.GroundTruth == "" || len(item.ExpectedConcepts) == 0 {
			return nil, fmt.Errorf("invalid scenario fixture %q", item.ID)
		}
		if _, exists := seen[item.ID]; exists {
			return nil, fmt.Errorf("duplicate scenario ID %q", item.ID)
		}
		seen[item.ID] = struct{}{}
		if only == "" || item.ID == only {
			selected = append(selected, item)
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("scenario %q was not found", only)
	}
	return selected, nil
}

func containsAll(values, required []string) bool {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	for _, value := range required {
		if _, ok := set[value]; !ok {
			return false
		}
	}
	return true
}

func matchesConcepts(value string, groups [][]string) bool {
	value = strings.ToLower(value)
	for _, alternatives := range groups {
		matched := false
		for _, alternative := range alternatives {
			if strings.Contains(value, strings.ToLower(alternative)) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func pause(duration time.Duration) {
	if duration > 0 {
		time.Sleep(duration)
	}
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func fatal(message string) {
	fmt.Fprintln(os.Stderr, "ERROR:", message)
	os.Exit(1)
}
