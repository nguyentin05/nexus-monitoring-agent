package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Telemetry struct {
	cfg        Config
	httpClient *http.Client
	kube       *KubeClient
}

func NewTelemetry(cfg Config, httpClient *http.Client) *Telemetry {
	return &Telemetry{cfg: cfg, httpClient: httpClient, kube: newKubeClient(cfg.HTTPTimeout)}
}

func (t *Telemetry) Collect(ctx context.Context, incident Incident, plan CollectionPlan) Evidence {
	evidence := incident.Evidence
	for _, step := range plan.Steps {
		switch step.Tool {
		case ToolServiceMetrics:
			if evidence.Metrics == nil {
				metrics, err := t.ServiceMetrics(ctx, incident.Service, step.Metrics)
				if err != nil {
					evidence.CollectionErrs = append(evidence.CollectionErrs, err.Error())
				} else {
					evidence.Metrics = &metrics
				}
			}
		case ToolErrorLogs:
			if len(evidence.Logs) == 0 {
				logs, err := t.filteredErrorLogs(ctx, incident.Service, time.Duration(step.LookbackMinutes)*time.Minute, step.LogTerms, step.Limit)
				if err != nil {
					evidence.CollectionErrs = append(evidence.CollectionErrs, err.Error())
				} else {
					evidence.Logs = logs
				}
			}
		case ToolWorkloadStatus:
			if evidence.Workload == nil {
				if t.kube == nil {
					evidence.CollectionErrs = append(evidence.CollectionErrs, "Kubernetes service account is unavailable")
				} else if workload, err := t.kube.WorkloadStatus(ctx, incident.Namespace, incident.Service); err != nil {
					evidence.CollectionErrs = append(evidence.CollectionErrs, err.Error())
				} else {
					evidence.Workload = &workload
				}
			}
		case ToolKubernetesEvents:
			if len(evidence.Events) == 0 {
				if t.kube == nil {
					evidence.CollectionErrs = append(evidence.CollectionErrs, "Kubernetes service account is unavailable")
				} else if events, err := t.kube.Events(ctx, incident.Namespace, incident.Service, incident.StartedAt, time.Duration(step.LookbackMinutes)*time.Minute, step.Limit); err != nil {
					evidence.CollectionErrs = append(evidence.CollectionErrs, err.Error())
				} else {
					evidence.Events = events
				}
			}
		case ToolNetworkPolicies:
			if len(evidence.NetworkPolicies) == 0 {
				if t.kube == nil {
					evidence.CollectionErrs = append(evidence.CollectionErrs, "Kubernetes service account is unavailable")
				} else if policies, err := t.kube.NetworkPolicies(ctx, incident.Namespace, incident.Service); err != nil {
					evidence.CollectionErrs = append(evidence.CollectionErrs, err.Error())
				} else {
					evidence.NetworkPolicies = policies
				}
			}
		case ToolTraceContext:
			if len(evidence.Traces) == 0 {
				if traces, err := t.TraceContext(ctx, incident.Service, incident.StartedAt, step); err != nil {
					evidence.CollectionErrs = append(evidence.CollectionErrs, err.Error())
				} else {
					evidence.Traces = traces
				}
			}
		case ToolRecentChanges:
			if len(evidence.RecentChanges) == 0 {
				if t.kube == nil {
					evidence.CollectionErrs = append(evidence.CollectionErrs, "Kubernetes service account is unavailable")
				} else if changes, err := t.kube.RecentChanges(ctx, incident.Namespace, incident.Service, incident.StartedAt, time.Duration(step.LookbackMinutes)*time.Minute, step.Limit); err != nil {
					evidence.CollectionErrs = append(evidence.CollectionErrs, err.Error())
				} else {
					evidence.RecentChanges = changes
				}
			}
		case ToolNodeStatus:
			if evidence.NodeStatus == nil {
				if t.kube == nil {
					evidence.CollectionErrs = append(evidence.CollectionErrs, "Kubernetes service account is unavailable")
				} else if status, err := t.kube.NodeStatus(ctx, incident.Service); err != nil {
					evidence.CollectionErrs = append(evidence.CollectionErrs, err.Error())
				} else {
					evidence.NodeStatus = &status
				}
			}
		}
	}
	return evidence
}

func (t *Telemetry) ServiceMetrics(ctx context.Context, service string, names []string) (MetricSnapshot, error) {
	namespace := strconv.Quote(t.cfg.Namespace)
	serviceLabel := strconv.Quote(service)
	pod := strconv.Quote(regexp.QuoteMeta(service) + "-.*")
	allQueries := map[string]string{
		"cpu":      fmt.Sprintf(`100 * sum(rate(container_cpu_usage_seconds_total{namespace=%s,pod=~%s,container!="",container!="POD"}[2m])) / clamp_min(sum(kube_pod_container_resource_limits{namespace=%s,pod=~%s,resource="cpu",unit="core"}), 0.001)`, namespace, pod, namespace, pod),
		"memory":   fmt.Sprintf(`sum(container_memory_working_set_bytes{namespace=%s,pod=~%s,container!="",container!="POD"}) / 1024 / 1024`, namespace, pod),
		"errors":   fmt.Sprintf(`100 * sum(rate(nexus_http_requests_total{namespace=%s,service=%s,status=~"5.."}[5m])) / clamp_min(sum(rate(nexus_http_requests_total{namespace=%s,service=%s}[5m])), 0.001)`, namespace, serviceLabel, namespace, serviceLabel),
		"latency":  fmt.Sprintf(`histogram_quantile(0.99, sum by (le) (rate(nexus_http_request_duration_seconds_bucket{namespace=%s,service=%s}[5m]))) * 1000`, namespace, serviceLabel),
		"restarts": fmt.Sprintf(`sum(increase(kube_pod_container_status_restarts_total{namespace=%s,pod=~%s}[15m]))`, namespace, pod),
	}
	queries := make(map[string]string, len(names))
	for _, name := range names {
		key := map[string]string{MetricCPU: "cpu", MetricMemory: "memory", MetricErrorRate: "errors", MetricP99: "latency", MetricRestarts: "restarts"}[name]
		queries[key] = allQueries[key]
	}

	type result struct {
		name  string
		value *float64
		err   error
	}
	results := make(chan result, len(queries))
	var wg sync.WaitGroup
	for name, query := range queries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value, err := t.prometheusQuery(ctx, query)
			results <- result{name: name, value: value, err: err}
		}()
	}
	wg.Wait()
	close(results)

	var snapshot MetricSnapshot
	var errs []string
	for result := range results {
		if result.err != nil {
			errs = append(errs, result.name+": "+result.err.Error())
			continue
		}
		switch result.name {
		case "cpu":
			snapshot.CPUPercent = result.value
		case "memory":
			snapshot.MemoryMB = result.value
		case "errors":
			snapshot.ErrorRatePercent = result.value
		case "latency":
			snapshot.P99LatencyMS = result.value
		case "restarts":
			snapshot.RestartCount = result.value
		}
	}
	if len(errs) == len(queries) {
		return snapshot, fmt.Errorf("Prometheus queries failed: %s", strings.Join(errs, "; "))
	}
	return snapshot, nil
}

func (t *Telemetry) prometheusQuery(ctx context.Context, query string) (*float64, error) {
	endpoint := t.cfg.PrometheusURL + "/api/v1/query?query=" + url.QueryEscape(query)
	var response struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := t.getJSON(ctx, endpoint, &response); err != nil {
		return nil, err
	}
	if response.Status != "success" || len(response.Data.Result) == 0 || len(response.Data.Result[0].Value) < 2 {
		return nil, nil
	}
	var raw string
	if err := json.Unmarshal(response.Data.Result[0].Value[1], &raw); err != nil {
		return nil, err
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func (t *Telemetry) ErrorLogs(ctx context.Context, service string, lookback time.Duration) ([]LogSample, error) {
	return t.filteredErrorLogs(ctx, service, lookback, nil, t.cfg.MaxLogSamples)
}

func (t *Telemetry) filteredErrorLogs(ctx context.Context, service string, lookback time.Duration, terms []string, limit int) ([]LogSample, error) {
	if len(terms) == 0 {
		terms = []string{"error", "exception", "traceback", "panic", "fatal", "timeout"}
	}
	escaped := make([]string, len(terms))
	for i, term := range terms {
		escaped[i] = regexp.QuoteMeta(term)
	}
	query := fmt.Sprintf(`{namespace=%q,container=%q} |~ "(?i)(%s)"`, t.cfg.Namespace, service, strings.Join(escaped, "|"))
	params := url.Values{
		"query":     []string{query},
		"start":     []string{strconv.FormatInt(time.Now().Add(-lookback).UnixNano(), 10)},
		"end":       []string{strconv.FormatInt(time.Now().UnixNano(), 10)},
		"limit":     []string{strconv.Itoa(limit)},
		"direction": []string{"backward"},
	}
	var response struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Stream map[string]string `json:"stream"`
				Values [][]string        `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := t.getJSON(ctx, t.cfg.LokiURL+"/loki/api/v1/query_range?"+params.Encode(), &response); err != nil {
		return nil, fmt.Errorf("Loki query failed: %w", err)
	}
	logs := make([]LogSample, 0, limit)
	for _, stream := range response.Data.Result {
		for _, value := range stream.Values {
			if len(value) < 2 || len(logs) >= limit {
				continue
			}
			ns, _ := strconv.ParseInt(value[0], 10, 64)
			logs = append(logs, LogSample{Timestamp: time.Unix(0, ns).UTC(), Pod: stream.Stream["pod"], Message: redact(value[1], 500)})
		}
	}
	return logs, nil
}

func (t *Telemetry) getJSON(ctx context.Context, endpoint string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	response, err := t.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("GET %s returned %s", request.URL.Path, response.Status)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(target)
}

type KubeClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

func newKubeClient(timeout time.Duration) *KubeClient {
	token, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return nil
	}
	ca, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if err != nil {
		return nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil
	}
	return &KubeClient{
		baseURL:    "https://kubernetes.default.svc",
		token:      strings.TrimSpace(string(token)),
		httpClient: &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}},
	}
}

func (k *KubeClient) get(ctx context.Context, path string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, k.baseURL+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+k.token)
	response, err := k.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Kubernetes API %s returned %s", path, response.Status)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(target)
}

func (k *KubeClient) AuthenticateServiceAccount(ctx context.Context, token, expectedUsername string) (bool, error) {
	payload, err := json.Marshal(map[string]any{
		"apiVersion": "authentication.k8s.io/v1",
		"kind":       "TokenReview",
		"spec":       map[string]string{"token": token},
	})
	if err != nil {
		return false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, k.baseURL+"/apis/authentication.k8s.io/v1/tokenreviews", bytes.NewReader(payload))
	if err != nil {
		return false, err
	}
	request.Header.Set("Authorization", "Bearer "+k.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := k.httpClient.Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return false, fmt.Errorf("Kubernetes TokenReview returned %s", response.Status)
	}
	var review struct {
		Status struct {
			Authenticated bool `json:"authenticated"`
			User          struct {
				Username string `json:"username"`
			} `json:"user"`
		} `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&review); err != nil {
		return false, err
	}
	return review.Status.Authenticated && review.Status.User.Username == expectedUsername, nil
}

func (k *KubeClient) WorkloadStatus(ctx context.Context, namespace, service string) (WorkloadStatus, error) {
	var deployment struct {
		Spec struct {
			Replicas *int32 `json:"replicas"`
		} `json:"spec"`
		Status struct {
			AvailableReplicas int32 `json:"availableReplicas"`
		} `json:"status"`
	}
	path := "/apis/apps/v1/namespaces/" + url.PathEscape(namespace) + "/deployments/" + url.PathEscape(service)
	if err := k.get(ctx, path, &deployment); err != nil {
		return WorkloadStatus{}, err
	}
	var pods struct {
		Items []struct {
			Status struct {
				Conditions        []struct{ Type, Status string } `json:"conditions"`
				ContainerStatuses []struct {
					RestartCount int32 `json:"restartCount"`
					LastState    struct {
						Terminated *struct {
							Reason string `json:"reason"`
						} `json:"terminated"`
					} `json:"lastState"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	podPath := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods?labelSelector=" + url.QueryEscape("app.kubernetes.io/name="+service)
	if err := k.get(ctx, podPath, &pods); err != nil {
		return WorkloadStatus{}, err
	}
	status := WorkloadStatus{AvailableReplicas: deployment.Status.AvailableReplicas, TotalPods: len(pods.Items)}
	if deployment.Spec.Replicas != nil {
		status.DesiredReplicas = *deployment.Spec.Replicas
	}
	for _, pod := range pods.Items {
		for _, condition := range pod.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				status.ReadyPods++
			}
		}
		for _, container := range pod.Status.ContainerStatuses {
			status.Restarts += container.RestartCount
			if container.LastState.Terminated != nil && container.LastState.Terminated.Reason != "" {
				status.TerminationReasons = append(status.TerminationReasons, container.LastState.Terminated.Reason)
			}
		}
	}
	return status, nil
}

func (k *KubeClient) NetworkPolicies(ctx context.Context, namespace, service string) ([]NetworkPolicyEvidence, error) {
	var response struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				PodSelector struct {
					MatchLabels map[string]string `json:"matchLabels"`
				} `json:"podSelector"`
				PolicyTypes []string `json:"policyTypes"`
				Egress      []struct {
					Ports []struct {
						Protocol string          `json:"protocol"`
						Port     json.RawMessage `json:"port"`
					} `json:"ports"`
				} `json:"egress"`
			} `json:"spec"`
		} `json:"items"`
	}
	path := "/apis/networking.k8s.io/v1/namespaces/" + url.PathEscape(namespace) + "/networkpolicies"
	if err := k.get(ctx, path, &response); err != nil {
		return nil, err
	}
	policies := make([]NetworkPolicyEvidence, 0, len(response.Items))
	for _, item := range response.Items {
		if item.Spec.PodSelector.MatchLabels["app.kubernetes.io/name"] != service {
			continue
		}
		policy := NetworkPolicyEvidence{Name: item.Metadata.Name, PolicyTypes: item.Spec.PolicyTypes, PodSelector: item.Spec.PodSelector.MatchLabels, EgressRuleCount: len(item.Spec.Egress)}
		for _, rule := range item.Spec.Egress {
			if len(rule.Ports) == 0 {
				policy.AllowedPorts = append(policy.AllowedPorts, "*")
			}
			for _, port := range rule.Ports {
				protocol := port.Protocol
				if protocol == "" {
					protocol = "TCP"
				}
				policy.AllowedPorts = append(policy.AllowedPorts, protocol+"/"+strings.Trim(string(port.Port), "\""))
			}
		}
		policies = append(policies, policy)
	}
	return policies, nil
}

type kubeEvent struct {
	Type          string    `json:"type"`
	Reason        string    `json:"reason"`
	Message       string    `json:"message"`
	EventTime     time.Time `json:"eventTime"`
	LastTimestamp time.Time `json:"lastTimestamp"`
	Metadata      struct {
		CreationTimestamp time.Time `json:"creationTimestamp"`
	} `json:"metadata"`
	Involved struct {
		Name string `json:"name"`
	} `json:"involvedObject"`
}

func (e kubeEvent) timestamp() time.Time {
	if !e.EventTime.IsZero() {
		return e.EventTime
	}
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp
	}
	return e.Metadata.CreationTimestamp
}

func (k *KubeClient) Events(ctx context.Context, namespace, service string, startedAt time.Time, lookback time.Duration, limit int) ([]KubernetesEvent, error) {
	var response struct {
		Items []kubeEvent `json:"items"`
	}
	path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/events?limit=100"
	if err := k.get(ctx, path, &response); err != nil {
		return nil, err
	}
	sort.Slice(response.Items, func(i, j int) bool {
		return response.Items[i].timestamp().After(response.Items[j].timestamp())
	})
	anchor := startedAt
	if anchor.IsZero() {
		anchor = time.Now()
	}
	cutoff := anchor.Add(-lookback)
	events := make([]KubernetesEvent, 0, limit)
	for _, event := range response.Items {
		if len(events) == cap(events) {
			break
		}
		timestamp := event.timestamp()
		if timestamp.Before(cutoff) || (event.Involved.Name != service && !strings.HasPrefix(event.Involved.Name, service+"-")) {
			continue
		}
		events = append(events, KubernetesEvent{Timestamp: timestamp, Type: event.Type, Reason: event.Reason, Object: event.Involved.Name, Message: redact(event.Message, 500)})
	}
	return events, nil
}

func incidentLookback(startedAt, now time.Time) time.Duration {
	if startedAt.IsZero() {
		return 5 * time.Minute
	}
	lookback := now.Sub(startedAt) + 2*time.Minute
	if lookback < 2*time.Minute {
		return 2 * time.Minute
	}
	if lookback > 5*time.Minute {
		return 5 * time.Minute
	}
	return lookback
}

type redactionRule struct {
	pattern     *regexp.Regexp
	replacement string
}

var redactionRules = []redactionRule{
	{regexp.MustCompile(`(?i)(https?://)[^/\s:@]+:[^/\s@]+@`), "$1[REDACTED]@"},
	{regexp.MustCompile(`(?i)authorization:\s*bearer\s+[A-Za-z0-9._-]+`), "Authorization: Bearer [REDACTED]"},
	{regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`), "[REDACTED:jwt]"},
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), "[REDACTED:aws_key]"},
	{regexp.MustCompile(`(?i)\b(password|passwd|pwd|token|secret|authorization|api[_-]?key)\s*[:=]\s*[^\s,;]+`), "$1=[REDACTED]"},
	{regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`), "[REDACTED:email]"},
}

func redact(value string, limit int) string {
	for _, rule := range redactionRules {
		value = rule.pattern.ReplaceAllString(value, rule.replacement)
	}
	if len(value) > limit {
		return value[:limit] + "..."
	}
	return value
}

func (t *Telemetry) TraceContext(ctx context.Context, service string, startedAt time.Time, step CollectionStep) ([]TraceEvidence, error) {
	anchor := startedAt
	if anchor.IsZero() {
		anchor = time.Now()
	}
	query := fmt.Sprintf(`{ resource.service.name = %q`, service)
	switch step.TraceStatus {
	case TraceStatusError:
		query += ` && status = error`
	case TraceStatusSlow:
		query += fmt.Sprintf(` && duration > %dms`, step.MinDurationMS)
	}
	query += ` }`
	params := url.Values{
		"q":     []string{query},
		"start": []string{strconv.FormatInt(anchor.Add(-time.Duration(step.LookbackMinutes)*time.Minute).Unix(), 10)},
		"end":   []string{strconv.FormatInt(anchor.Add(2*time.Minute).Unix(), 10)},
		"limit": []string{strconv.Itoa(step.Limit)},
	}
	var response struct {
		Traces []struct {
			TraceID         string  `json:"traceID"`
			RootServiceName string  `json:"rootServiceName"`
			RootTraceName   string  `json:"rootTraceName"`
			StartTime       string  `json:"startTimeUnixNano"`
			DurationMS      float64 `json:"durationMs"`
		} `json:"traces"`
	}
	if err := t.getJSON(ctx, t.cfg.TempoURL+"/api/search?"+params.Encode(), &response); err != nil {
		return nil, fmt.Errorf("Tempo query failed: %w", err)
	}
	traces := make([]TraceEvidence, 0, len(response.Traces))
	for _, item := range response.Traces {
		started := time.Time{}
		if ns, err := strconv.ParseInt(item.StartTime, 10, 64); err == nil {
			started = time.Unix(0, ns).UTC()
		}
		traces = append(traces, TraceEvidence{TraceID: item.TraceID, RootService: item.RootServiceName, RootName: redact(item.RootTraceName, 200), StartedAt: started, DurationMS: item.DurationMS})
	}
	return traces, nil
}

func (k *KubeClient) RecentChanges(ctx context.Context, namespace, service string, startedAt time.Time, lookback time.Duration, limit int) ([]DeploymentChange, error) {
	var response struct {
		Items []struct {
			Metadata struct {
				Name              string            `json:"name"`
				CreationTimestamp time.Time         `json:"creationTimestamp"`
				Annotations       map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec struct {
				Replicas *int32 `json:"replicas"`
				Template struct {
					Spec struct {
						Containers []struct {
							Image string `json:"image"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
			Status struct {
				ReadyReplicas int32 `json:"readyReplicas"`
			} `json:"status"`
		} `json:"items"`
	}
	path := "/apis/apps/v1/namespaces/" + url.PathEscape(namespace) + "/replicasets?labelSelector=" + url.QueryEscape("app.kubernetes.io/name="+service)
	if err := k.get(ctx, path, &response); err != nil {
		return nil, err
	}
	anchor := startedAt
	if anchor.IsZero() {
		anchor = time.Now()
	}
	cutoff := anchor.Add(-lookback)
	sort.Slice(response.Items, func(i, j int) bool {
		return response.Items[i].Metadata.CreationTimestamp.After(response.Items[j].Metadata.CreationTimestamp)
	})
	changes := make([]DeploymentChange, 0, limit)
	for _, item := range response.Items {
		if len(changes) >= limit || item.Metadata.CreationTimestamp.Before(cutoff) || item.Metadata.CreationTimestamp.After(anchor.Add(2*time.Minute)) {
			continue
		}
		change := DeploymentChange{ReplicaSet: item.Metadata.Name, Revision: item.Metadata.Annotations["deployment.kubernetes.io/revision"], CreatedAt: item.Metadata.CreationTimestamp, Ready: item.Status.ReadyReplicas}
		if item.Spec.Replicas != nil {
			change.Replicas = *item.Spec.Replicas
		}
		for _, container := range item.Spec.Template.Spec.Containers {
			change.Images = append(change.Images, container.Image)
		}
		changes = append(changes, change)
	}
	return changes, nil
}

func (k *KubeClient) NodeStatus(ctx context.Context, name string) (NodeStatusEvidence, error) {
	var node struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Taints []struct{ Key, Value, Effect string } `json:"taints"`
		} `json:"spec"`
		Status struct {
			Conditions  []struct{ Type, Status, Reason string } `json:"conditions"`
			Capacity    map[string]string                       `json:"capacity"`
			Allocatable map[string]string                       `json:"allocatable"`
		} `json:"status"`
	}
	if err := k.get(ctx, "/api/v1/nodes/"+url.PathEscape(name), &node); err != nil {
		return NodeStatusEvidence{}, err
	}
	result := NodeStatusEvidence{Name: node.Metadata.Name, Conditions: make(map[string]string), Capacity: node.Status.Capacity, Allocatable: node.Status.Allocatable}
	for _, condition := range node.Status.Conditions {
		result.Conditions[condition.Type] = condition.Status + ": " + condition.Reason
	}
	for _, taint := range node.Spec.Taints {
		result.Taints = append(result.Taints, taint.Key+"="+taint.Value+":"+taint.Effect)
	}
	return result, nil
}
