package execwindow

import (
	"context"
	"os"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/log"
)

// Environment names the node and cluster the records belong to. Both set turns recording on; the provider writes nothing without them.
const (
	EnvCluster  = "VZ_EXECUTION_CLUSTER"
	EnvNode     = "VZ_EXECUTION_NODE"
	EnvEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"
)

// Meter reads the VM's host process.
type Meter interface {
	Read() (Reading, error)
}

// Finder resolves the VM's host process once it exists. A nil Meter with a nil error means not yet.
type Finder func() (Meter, error)

// Recorder writes the windows of every VM it tracks.
type Recorder struct {
	cluster, node string
	sink          Sink
	now           func() time.Time
	every         time.Duration
}

// FromEnv returns the recorder the environment asks for, or nil when recording is off.
func FromEnv() *Recorder {
	cluster, node := os.Getenv(EnvCluster), os.Getenv(EnvNode)
	if cluster == "" || node == "" {
		return nil
	}
	return NewRecorder(cluster, node, OTLP{Endpoint: os.Getenv(EnvEndpoint)})
}

// NewRecorder returns a recorder for the Pods of one node.
func NewRecorder(cluster, node string, sink Sink) *Recorder {
	return &Recorder{cluster: cluster, node: node, sink: sink, now: time.Now, every: SampleEvery}
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
			if reading, err := meter.Read(); err == nil {
				reading.At = now
				w.Observe(reading)
			} else {
				logger.WithError(err).Debug("VM host process not readable")
			}
		}
		if stopped() {
			flush(now)
			return
		}
		if w.Due(now) {
			flush(now)
		}
		<-tick.C
	}
}
