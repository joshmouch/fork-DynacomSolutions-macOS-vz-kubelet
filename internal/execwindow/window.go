// Package execwindow writes one bounded-execution window record per interval of a macOS VM's life (ai-fleet-protocol
// execution-limits 2.1.0, the record shape ai-fleet-cluster's ai_fleet_execution.bounded_process_runs reads from any log
// record whose payload.limits carries execution.id). The VM is the Pod's boundary: the hypervisor holds it to the vCPU count and
// memory it was built with, so those are the record's cpu.peak and memory.peak bounds; usage is what the VM's host process used.
package execwindow

import (
	"fmt"
	"time"
)

const (
	// Contract is the execution-limits version these records follow: every usage cell states its status and method.
	Contract = "2.1.0"
	// Binding names the observer: the VM's host process read against the VM's own configuration.
	Binding = "vz-vm"
	// Level is the execution tree level of one Pod.
	Level = "pod"
	// WindowEvery is the window length, as ai-fleet's sampler writes for an execution.
	WindowEvery = 10 * time.Second
	// SampleEvery is the reading interval; a cell folded from readings is sample:<this in ms>.
	SampleEvery = time.Second

	counter = "counter"
	sampled = "sample:1000"
)

// Subject is one execution: a Pod's VM, with the bounds it was built with.
type Subject struct {
	ID          string // pod:<cluster>/<namespace>/<name>
	Parent      string // node:<cluster>/<node>
	VCPU        uint
	MemoryBytes uint64
	Started     time.Time
}

// Reading is one observation of the VM's host process. Counters are cumulative since the process began, which is the VM's boot.
type Reading struct {
	At                   time.Time
	CPUNanos             uint64 // user + system CPU time
	Footprint            uint64 // physical footprint now, bytes
	LifetimeMaxFootprint uint64 // the kernel's high-water mark of the footprint since the process began, bytes
}

// Window folds readings into one record. A window starts where the previous one ended, so consecutive windows tile the VM's life
// and a lifetime total is the sum of its windows.
type Window struct {
	subject Subject
	start   time.Time

	baseCPU    uint64
	markStart  uint64
	last, prev *Reading
	peak       uint64
	cpuPeak    float64
	haveCPU    bool
	areaMs     float64 // footprint integrated over time, byte-milliseconds
}

// First returns the window that starts at the VM's start: the process began then, so its counters count from zero.
func First(s Subject) *Window { return &Window{subject: s, start: s.Started} }

// Observe folds one reading into the window.
func (w *Window) Observe(r Reading) {
	if w.last != nil && r.At.After(w.last.At) {
		dt := r.At.Sub(w.last.At)
		if r.CPUNanos >= w.last.CPUNanos {
			if cores := float64(r.CPUNanos-w.last.CPUNanos) / float64(dt.Nanoseconds()); !w.haveCPU || cores > w.cpuPeak {
				w.cpuPeak, w.haveCPU = cores, true
			}
		}
		w.areaMs += float64(w.last.Footprint+r.Footprint) / 2 * float64(dt.Milliseconds())
	}
	if r.Footprint > w.peak {
		w.peak = r.Footprint
	}
	cp := r
	w.last = &cp
}

// Due reports whether the window is old enough to write.
func (w *Window) Due(now time.Time) bool { return now.Sub(w.start) >= WindowEvery }

// Record returns the window's flat map (the payload.limits of the envelope) ending at end, and the window that follows it.
func (w *Window) Record(end time.Time) (map[string]any, *Window) {
	s := w.subject
	out := map[string]any{
		"execution.id": s.ID, "relation.parent": s.Parent, "execution.level": Level, "execution.binding": Binding,
		"execution.started_at_ms": w.start.UnixMilli(), "execution.ended_at_ms": end.UnixMilli(),
		"usage.wallTime.total": end.Sub(w.start).Milliseconds(), "usage_method.wallTime.total": counter, "usage_status.wallTime.total": "complete",
		"bound.cpu.peak": s.VCPU, "bound.memory.peak": s.MemoryBytes, "bound.contract": Contract,
	}
	next := &Window{subject: s, start: end}
	if w.last == nil {
		const why = "unavailable:vm-process-not-observed"
		for _, cell := range []string{"cpu.total", "memory.peak", "memory.total"} {
			out["usage_status."+cell] = why
		}
		return out, next
	}
	out["usage.cpu.total"] = float64(w.last.CPUNanos-min(w.baseCPU, w.last.CPUNanos)) / 1e6 // milliseconds of CPU time
	out["usage_method.cpu.total"], out["usage_status.cpu.total"] = counter, "complete"
	if w.haveCPU {
		out["usage.cpu.peak"], out["usage_method.cpu.peak"], out["usage_status.cpu.peak"] = w.cpuPeak, sampled, "partial:sampled"
	}
	// The kernel's lifetime mark is the window's peak when it rose inside the window; otherwise the highest reading.
	peak, method, status := w.peak, sampled, "partial:sampled"
	if w.last.LifetimeMaxFootprint > w.markStart {
		peak, method, status = max(peak, w.last.LifetimeMaxFootprint), counter, "complete"
	}
	out["usage.memory.peak"], out["usage_method.memory.peak"], out["usage_status.memory.peak"] = peak, method, status
	out["usage.memory.total"] = w.areaMs / 1000 // byte-seconds
	out["usage_method.memory.total"], out["usage_status.memory.total"] = sampled, "partial:sampled"
	next.baseCPU, next.markStart, next.prev = w.last.CPUNanos, w.last.LifetimeMaxFootprint, w.last
	next.last = w.last
	next.peak = w.last.Footprint
	return out, next
}

// PodID is the id of a Pod's execution row: pod:<cluster>/<namespace>/<name>.
func PodID(cluster, namespace, name string) string {
	return fmt.Sprintf("pod:%s/%s/%s", cluster, namespace, name)
}

// NodeID is the id of the node that holds the Pod: node:<cluster>/<node>.
func NodeID(cluster, node string) string { return fmt.Sprintf("node:%s/%s", cluster, node) }
