package execwindow

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/log"
)

// Environment names the node and cluster the records belong to. Both set turns recording on; the provider writes nothing without them.
const (
	EnvCluster  = "VZ_EXECUTION_CLUSTER"
	EnvNode     = "VZ_EXECUTION_NODE"
	EnvEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"
	// EnvNodeParent is the node row's parent, the physical host the provider runs on (host:<machine>/<os>); empty writes none.
	EnvNodeParent = "VZ_EXECUTION_NODE_PARENT"
)

// Meter reads the VM's host process.
type Meter interface {
	Read() (Reading, error)
}

// Finder resolves the VM's host process once it exists. A nil Meter with a nil error means not yet.
type Finder func() (Meter, error)

// Recorder writes the windows of every VM it tracks, and the node's own (RunNode).
type Recorder struct {
	cluster, node, nodeParent string
	sink                      Sink
	now                       func() time.Time
	every                     time.Duration
	self                      func() (Meter, error) // this provider's own process (SelfMeter)

	mu  sync.Mutex
	vms map[string]Meter // each tracked Pod's resolved VM host process, by Pod id: the node counts them with its own process
}

// FromEnv returns the recorder the environment asks for, or nil when recording is off.
func FromEnv() *Recorder {
	cluster, node := os.Getenv(EnvCluster), os.Getenv(EnvNode)
	if cluster == "" || node == "" {
		return nil
	}
	r := NewRecorder(cluster, node, OTLP{Endpoint: os.Getenv(EnvEndpoint)})
	r.nodeParent = os.Getenv(EnvNodeParent)
	return r
}

// NewRecorder returns a recorder for the Pods of one node.
func NewRecorder(cluster, node string, sink Sink) *Recorder {
	return &Recorder{cluster: cluster, node: node, sink: sink, now: time.Now, every: SampleEvery, self: SelfMeter, vms: map[string]Meter{}}
}

// nodeMeter reads the virtual node's whole use of its host: this provider's process and every VM host process a tracked Pod
// resolved. Each process's CPU counts from its first reading here, and a process that is gone keeps what it used, so the node's
// counter only grows and a Pod's CPU is inside the node's; the footprint is the processes' sum now.
type nodeMeter struct {
	r    *Recorder
	self Meter
	last map[string]uint64 // each process's CPU at its previous reading
	cpu  uint64
}

func (m *nodeMeter) Read() (Reading, error) {
	own, err := m.self.Read()
	if err != nil {
		return Reading{}, err
	}
	m.r.mu.Lock()
	vms := make(map[string]Meter, len(m.r.vms))
	for id, vm := range m.r.vms {
		vms[id] = vm
	}
	m.r.mu.Unlock()
	readings := map[string]Reading{"": own} // "" is the provider's own process
	for id, vm := range vms {
		if reading, err := vm.Read(); err == nil {
			readings[id] = reading
		}
	}
	footprint := uint64(0)
	seen := map[string]uint64{}
	for id, reading := range readings {
		if prior, ok := m.last[id]; !ok {
			if m.last != nil && id != "" { // a VM that began since the node's first reading: all its CPU is the node's
				m.cpu += reading.CPUNanos
			}
		} else if reading.CPUNanos >= prior {
			m.cpu += reading.CPUNanos - prior
		}
		seen[id] = reading.CPUNanos
		footprint += reading.Footprint
	}
	m.last = seen
	return Reading{At: own.At, CPUNanos: m.cpu, Footprint: footprint}, nil
}

// RunNode writes the virtual node's windows (node:<cluster>/<node>, parent the host from VZ_EXECUTION_NODE_PARENT) every
// WindowEvery until ctx ends: its usage is the provider's own process and the VMs it runs, the Pods' parent. A nil Recorder runs
// nothing.
func (r *Recorder) RunNode(ctx context.Context) {
	if r == nil {
		return
	}
	logger := log.G(ctx).WithField("node", r.node)
	self, err := r.self()
	if err != nil {
		logger.WithError(err).Warn("node execution windows off: this process is not readable")
		return
	}
	meter := &nodeMeter{r: r, self: self}
	subject := Subject{ID: NodeID(r.cluster, r.node), Parent: r.nodeParent, Level: NodeLevel, Binding: NodeBinding}
	var w *Window
	tick := time.NewTicker(r.every)
	defer tick.Stop()
	for {
		now := r.now()
		reading, err := meter.Read()
		reading.At = now
		switch {
		case err != nil:
			logger.WithError(err).Debug("node processes not readable")
		case w == nil:
			w = Resume(subject, reading)
		default:
			w.Observe(reading)
		}
		if w != nil && w.Due(now) {
			var limits map[string]any
			limits, w = w.Record(now)
			if err := r.sink.Emit(ctx, limits, now); err != nil {
				logger.WithError(err).Warn("node execution window not delivered")
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Track follows one Pod's VM from its start (when its host process, and so its counters, began) until stopped reports it has stopped, writing a window every WindowEvery and a final
// one at the end. It returns when the final window is written. A nil Recorder tracks nothing.
func (r *Recorder) Track(ctx context.Context, namespace, name string, vcpu uint, memoryBytes uint64, started time.Time, find Finder, stopped func() bool) {
	if r == nil {
		return
	}
	ctx = context.WithoutCancel(ctx) // the Pod's context ends with its deletion; the final window is written after it
	logger := log.G(ctx).WithField("pod", namespace+"/"+name)
	w := First(Subject{ID: PodID(r.cluster, namespace, name), Parent: NodeID(r.cluster, r.node), VCPU: vcpu, MemoryBytes: memoryBytes, Started: started})
	var meter Meter
	flush := func(end time.Time) {
		var limits map[string]any
		limits, w = w.Record(end)
		if err := r.sink.Emit(ctx, limits, end); err != nil {
			logger.WithError(err).Warn("execution window not delivered")
		}
	}
	tick := time.NewTicker(r.every)
	defer tick.Stop()
	for {
		now := r.now()
		if meter == nil && find != nil {
			m, err := find()
			if err != nil {
				logger.WithError(err).Debug("VM host process not resolved yet")
			}
			meter = m
		}
		if meter != nil {
			r.mu.Lock()
			r.vms[w.subject.ID] = meter
			r.mu.Unlock()
			if reading, err := meter.Read(); err == nil {
				reading.At = now
				w.Observe(reading)
			} else {
				logger.WithError(err).Debug("VM host process not readable")
			}
		}
		if stopped() {
			flush(now)
			r.mu.Lock()
			delete(r.vms, w.subject.ID)
			r.mu.Unlock()
			return
		}
		if w.Due(now) {
			flush(now)
		}
		<-tick.C
	}
}
