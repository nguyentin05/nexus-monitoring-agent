package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

type Discord struct {
	webhookURL string
	client     *http.Client
}

func NewDiscord(webhookURL string, client *http.Client) *Discord {
	return &Discord{webhookURL: webhookURL, client: client}
}

func (d *Discord) Send(ctx context.Context, outcome Outcome) error {
	if d.webhookURL == "" {
		raw, _ := json.Marshal(outcome)
		slog.Info("Discord webhook is not configured", "outcome", string(raw))
		return nil
	}
	fields := []map[string]any{
		{"name": "Service", "value": outcome.Incident.Service, "inline": true},
		{"name": "Alert", "value": outcome.Incident.AlertName, "inline": true},
		{"name": "Processing path", "value": outcome.Path, "inline": true},
	}
	if outcome.RCA != nil {
		fields = append(fields,
			map[string]any{"name": "Root cause", "value": limit(outcome.RCA.RootCause, 1024), "inline": false},
			map[string]any{"name": "Confidence", "value": outcome.RCA.Confidence, "inline": true},
			map[string]any{"name": "Suggested actions", "value": limit(strings.Join(outcome.RCA.SuggestedActions, "\n"), 1024), "inline": false},
		)
	}
	if outcome.Fallback {
		fields = append(fields, map[string]any{"name": "AI analysis unavailable", "value": limit(outcome.Error, 1024), "inline": false})
	}
	payload := map[string]any{"embeds": []map[string]any{{
		"title":       "Nexus monitoring incident - " + outcome.Incident.Service,
		"description": limit(outcome.Incident.Description, 2048),
		"color":       discordColor(outcome),
		"fields":      fields,
		"timestamp":   outcome.Incident.StartedAt.Format("2006-01-02T15:04:05Z07:00"),
		"footer":      map[string]string{"text": "Nexus Monitoring Agent | LLM RCA"},
	}}}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, d.webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := d.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return fmt.Errorf("Discord returned %s: %s", response.Status, strings.TrimSpace(string(message)))
	}
	return nil
}

func discordColor(outcome Outcome) int {
	if outcome.Fallback {
		return 0x3498DB
	}
	if outcome.Incident.Severity == "critical" {
		return 0xE74C3C
	}
	return 0xF39C12
}

func limit(value string, max int) string {
	if max < 1 {
		panic(errors.New("max must be positive"))
	}
	if len(value) <= max {
		return value
	}
	return value[:max-3] + "..."
}
