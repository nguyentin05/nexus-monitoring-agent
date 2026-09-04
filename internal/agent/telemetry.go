package agent

import (
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
	for _, tool := range plan.Tools {
		switch tool {
		case ToolServiceMetrics:
			if evidence.Metrics == nil {
				metrics, err := t.ServiceMetrics(ctx, incident.Service)
				if err != nil {
					evidence.CollectionErrs = append(evidence.CollectionErrs, err.Error())
				} else {
					evidence.Metrics = &metrics
				}
			}
		case ToolErrorLogs:
			if len(evidence.Logs) == 0 {
				logs, err := t.ErrorLogs(ctx, incident.Service, 5*time.Minute)
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
				} else if events, err := t.kube.Events(ctx, incident.Namespace, incident.Service); err != nil {
					evidence.CollectionErrs = append(evidence.CollectionErrs, err.Error())
				} else {
					evidence.Events = events
				}
			}
		}
	}
	return evidence
}

func (t *Telemetry) ServiceMetrics(ctx context.Context, service string) (MetricSnapshot, error) {
	namespace := strconv.Quote(t.cfg.Namespace)
	serviceLabel := strconv.Quote(service)
	pod := strconv.Quote(regexp.QuoteMeta(service) + "-.*")
	queries := map[string]string{
		"cpu":      fmt.Sprintf(`100 * sum(rate(container_cpu_usage_seconds_total{namespace=%s,pod=~%s,container!="",container!="POD"}[2m])) / clamp_min(sum(kube_pod_container_resource_limits{namespace=%s,pod=~%s,resource="cpu",unit="core"}), 0.001)`, namespace, pod, namespace, pod),
		"memory":   fmt.Sprintf(`sum(container_memory_working_set_bytes{namespace=%s,pod=~%s,container!="",container!="POD"}) / 1024 / 1024`, namespace, pod),
		"errors":   fmt.Sprintf(`100 * sum(rate(nexus_http_requests_total{namespace=%s,service=%s,status=~"5.."}[5m])) / clamp_min(sum(rate(nexus_http_requests_total{namespace=%s,service=%s}[5m])), 0.001)`, namespace, serviceLabel, namespace, serviceLabel),
		"latency":  fmt.Sprintf(`histogram_quantile(0.99, sum by (le) (rate(nexus_http_request_duration_seconds_bucket{namespace=%s,service=%s}[5m]))) * 1000`, namespace, serviceLabel),
		"restarts": fmt.Sprintf(`sum(increase(kube_pod_container_status_restarts_total{namespace=%s,pod=~%s}[15m]))`, namespace, pod),
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
	query := fmt.Sprintf(`{namespace=%q,container=%q} |~ "(?i)(error|exception|traceback|panic|fatal|timeout)"`, t.cfg.Namespace, service)
	params := url.Values{
		"query":     []string{query},
		"start":     []string{strconv.FormatInt(time.Now().Add(-lookback).UnixNano(), 10)},
		"end":       []string{strconv.FormatInt(time.Now().UnixNano(), 10)},
		"limit":     []string{strconv.Itoa(t.cfg.MaxLogSamples)},
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
	logs := make([]LogSample, 0, t.cfg.MaxLogSamples)
	for _, stream := range response.Data.Result {
		for _, value := range stream.Values {
			if len(value) < 2 || len(logs) >= t.cfg.MaxLogSamples {
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

func (k *KubeClient) Events(ctx context.Context, namespace, service string) ([]KubernetesEvent, error) {
	var response struct {
		Items []struct {
			Type     string `json:"type"`
			Reason   string `json:"reason"`
			Message  string `json:"message"`
			Involved struct {
				Name string `json:"name"`
			} `json:"involvedObject"`
		} `json:"items"`
	}
	path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/events?limit=100"
	if err := k.get(ctx, path, &response); err != nil {
		return nil, err
	}
	events := make([]KubernetesEvent, 0, 10)
	for i := len(response.Items) - 1; i >= 0 && len(events) < 10; i-- {
		event := response.Items[i]
		if !strings.HasPrefix(event.Involved.Name, service) {
			continue
		}
		events = append(events, KubernetesEvent{Type: event.Type, Reason: event.Reason, Object: event.Involved.Name, Message: redact(event.Message, 500)})
	}
	return events, nil
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
