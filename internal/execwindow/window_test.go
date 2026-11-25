package execwindow

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func subject() Subject {
	return Subject{ID: PodID("ai-fleet", "arc-macos", "runner-1"), Parent: NodeID("ai-fleet", "macos-josh-mac"), VCPU: 4, MemoryBytes: 8 << 30, Started: t0}
}

func reading(sec int, cpuMs, footprintMiB, markMiB uint64) Reading {
	return Reading{At: t0.Add(time.Duration(sec) * time.Second), CPUNanos: cpuMs * 1e6, Footprint: footprintMiB << 20, LifetimeMaxFootprint: markMiB << 20}
}

func TestPodWindowCarriesBoundsAndUsage(t *testing.T) {
	w := First(subject())
	w.Observe(reading(1, 1500, 1000, 1200))
	w.Observe(reading(2, 3000, 1100, 1200))
	rec, next := w.Record(t0.Add(2 * time.Second))

	assert.Equal(t, "pod:ai-fleet/arc-macos/runner-1", rec["execution.id"])
	assert.Equal(t, "node:ai-fleet/macos-josh-mac", rec["relation.parent"])
	assert.Equal(t, "pod", rec["execution.level"])
	assert.Equal(t, uint(4), rec["bound.cpu.peak"])
	assert.Equal(t, uint64(8<<30), rec["bound.memory.peak"])
	assert.Equal(t, "2.1.0", rec["bound.contract"])
	assert.Equal(t, int64(2000), rec["usage.wallTime.total"])
	assert.InDelta(t, 3000.0, rec["usage.cpu.total"], 1e-6) // ms of CPU time since the VM began
	assert.InDelta(t, 1.5, rec["usage.cpu.peak"], 1e-9)     // 1500 ms of CPU in a 1 s interval
	assert.Equal(t, "sample:1000", rec["usage_method.cpu.peak"])
	assert.Equal(t, float64(1200<<20), float64(rec["usage.memory.peak"].(uint64))) // the kernel's lifetime mark rose in the window
	assert.Equal(t, "counter", rec["usage_method.memory.peak"])
	assert.InDelta(t, (1050.0*(1<<20))*1, rec["usage.memory.total"], 1e-3) // 1000 -> 1100 MiB over the second interval: 1050 MiB x 1 s
	assertEveryUsageCellStatesItsMethod(t, rec)

	// The next window starts where this one ended and counts CPU from there.
	next.Observe(reading(3, 3600, 1100, 1200))
	rec2, _ := next.Record(t0.Add(3 * time.Second))
	assert.Equal(t, t0.Add(2*time.Second).UnixMilli(), rec2["execution.started_at_ms"])
	assert.InDelta(t, 600.0, rec2["usage.cpu.total"], 1e-6)
	assert.Equal(t, "sample:1000", rec2["usage_method.memory.peak"]) // the lifetime mark did not rise: a sampled peak
	assert.Equal(t, "partial:sampled", rec2["usage_status.memory.peak"])
}

func TestUnobservedProcessIsNotZero(t *testing.T) {
	rec, _ := First(subject()).Record(t0.Add(time.Second))
	assert.NotContains(t, rec, "usage.cpu.total")
	assert.NotContains(t, rec, "usage.memory.peak")
	assert.Equal(t, "unavailable:vm-process-not-observed", rec["usage_status.memory.peak"])
	assert.Equal(t, uint(4), rec["bound.cpu.peak"])
}

func assertEveryUsageCellStatesItsMethod(t *testing.T, rec map[string]any) {
	t.Helper()
	for key := range rec {
		if len(key) > 6 && key[:6] == "usage." {
			assert.Contains(t, rec, "usage_method."+key[6:], key)
			assert.Contains(t, rec, "usage_status."+key[6:], key)
		}
	}
}

type fakeMeter struct{ readings []Reading }

func (m *fakeMeter) Read() (Reading, error) {
	r := m.readings[0]
	if len(m.readings) > 1 {
		m.readings = m.readings[1:]
	}
	return r, nil
}

type collect struct{ got []map[string]any }

func (c *collect) Emit(_ context.Context, limits map[string]any, _ time.Time) error {
	c.got = append(c.got, limits)
	return nil
}

func TestTrackWritesWindowsAndAFinalOne(t *testing.T) {
	sink := &collect{}
	r := NewRecorder("ai-fleet", "macos-josh-mac", sink)
	r.every = time.Millisecond
	clock := t0
	r.now = func() time.Time { clock = clock.Add(time.Second); return clock } // each loop is one second of VM life
	meter := &fakeMeter{readings: []Reading{reading(0, 1000, 500, 600), reading(0, 2000, 500, 600)}}
	calls := 0
	r.Track(context.Background(), "arc-macos", "runner-1", 4, 8<<30, t0, func() (Meter, error) { return meter, nil }, func() bool { calls++; return calls > 12 })

	require.GreaterOrEqual(t, len(sink.got), 2) // a window after 10 s and the final one
	for _, rec := range sink.got {
		assert.Equal(t, "pod:ai-fleet/arc-macos/runner-1", rec["execution.id"])
	}
}

func TestNilRecorderTracksNothing(t *testing.T) {
	var r *Recorder
	r.Track(context.Background(), "ns", "pod", 1, 1, t0, nil, func() bool { return true })
}

func TestEmitPostsAnExecutionTelemetryLine(t *testing.T) {
	var path string
	var doc map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path = req.URL.Path
		b, _ := io.ReadAll(req.Body)
		require.NoError(t, json.Unmarshal(b, &doc))
	}))
	defer srv.Close()
	rec, _ := First(subject()).Record(t0.Add(time.Second))
	require.NoError(t, OTLP{Endpoint: srv.URL}.Emit(context.Background(), rec, t0.Add(time.Second)))

	assert.Equal(t, "/v1/logs", path)
	logRecord := doc["resourceLogs"].([]any)[0].(map[string]any)["scopeLogs"].([]any)[0].(map[string]any)["logRecords"].([]any)[0].(map[string]any)
	var line struct {
		Schema string
		Event  struct {
			EventType string
			Payload   struct{ Limits map[string]any }
		}
	}
	require.NoError(t, json.Unmarshal([]byte(logRecord["body"].(map[string]any)["stringValue"].(string)), &line))
	assert.Equal(t, "ai-fleet.execution-telemetry-line/v1", line.Schema)
	assert.Equal(t, "execution-window", line.Event.EventType)
	assert.Equal(t, "pod:ai-fleet/arc-macos/runner-1", line.Event.Payload.Limits["execution.id"])
	assert.EqualValues(t, 4, line.Event.Payload.Limits["bound.cpu.peak"])
}
