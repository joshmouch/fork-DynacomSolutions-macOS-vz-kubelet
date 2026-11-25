package execwindow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// Service is the producer's name on every record.
	Service = "macos-vz-kubelet"
	// EventType is the record kind the cluster's sampler uses for a window.
	EventType = "execution-window"
	// DefaultEndpoint is the machine's edge Collector (the same default the build-step emitter uses).
	DefaultEndpoint = "http://127.0.0.1:4318"
)

// Sink delivers one window's flat map.
type Sink interface {
	Emit(ctx context.Context, limits map[string]any, at time.Time) error
}

// OTLP posts a window as one OTLP/HTTP JSON log record to the machine's edge Collector: an execution-telemetry line whose
// event.payload.limits is the flat map. The Collector lifts that map into the record's attributes and the cluster's
// bounded_process_runs reader takes it from there; nothing else reads or stores it.
type OTLP struct {
	Endpoint string
	Client   *http.Client
}

// Emit posts the record; delivery is best-effort, as every execution-telemetry line declares.
func (o OTLP) Emit(ctx context.Context, limits map[string]any, at time.Time) error {
	id, _ := limits["execution.id"].(string)
	started, _ := limits["execution.started_at_ms"].(int64)
	eventID := id + "/" + strconv.FormatInt(started, 10)
	atMs := at.UnixMilli()
	line := map[string]any{"schema": "ai-fleet.execution-telemetry-line/v1", "at": at.UTC().Format(time.RFC3339Nano), "event": map[string]any{
		"id": eventID, "eventType": EventType,
		"from":    map[string]any{"namespace": "service", "value": Service},
		"to":      map[string]any{"namespace": "record", "value": id},
		"order":   map[string]any{"stream": id, "sequence": atMs, "observedAt": atMs},
		"kind":    map[string]any{"record": "log", "severity": "info"},
		"payload": map[string]any{"limits": limits}, "correlation": "service/" + Service + "/" + id, "deliveryPolicy": "best-effort"}}
	body, err := json.Marshal(line)
	if err != nil {
		return err
	}
	str := func(k, v string) map[string]any {
		return map[string]any{"key": k, "value": map[string]any{"stringValue": v}}
	}
	nanos := strconv.FormatInt(atMs*1_000_000, 10)
	doc := map[string]any{"resourceLogs": []any{map[string]any{
		"resource": map[string]any{"attributes": []any{str("service.name", Service)}},
		"scopeLogs": []any{map[string]any{"scope": map[string]any{"name": Service + "/execwindow"}, "logRecords": []any{map[string]any{
			"timeUnixNano": nanos, "observedTimeUnixNano": nanos, "severityNumber": 9, "severityText": "INFO",
			"body":       map[string]any{"stringValue": string(body)},
			"attributes": []any{str("ai_fleet.event_type", EventType), str("ai_fleet.event_id", eventID), str("ai_fleet.event_stream", id), str("ai_fleet.subject", id)},
		}}}}}}}
	payload, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	endpoint := strings.TrimSuffix(o.Endpoint, "/")
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	client := o.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/logs", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("OTLP %s answered %d: %s", endpoint, resp.StatusCode, excerpt)
	}
	return nil
}
