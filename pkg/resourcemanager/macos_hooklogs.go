package resourcemanager

import (
	"bytes"
	"io"
	"sync"
)

// The kubelet's log endpoint has no container runtime behind a macOS VM, so the output of the pod's post-start
// hook (the Job's command: the hook is the job) is the pod's log. It is kept per pod, bounded, and outlives the
// VM, because `kubectl logs` of a finished pod is read after the VM has stopped.
const (
	// maxHookLogBytes bounds one pod's retained output; older bytes are dropped first.
	maxHookLogBytes = 1 << 20
	// maxHookLogPods bounds how many pods' logs are retained; the oldest pod's is dropped first.
	maxHookLogPods = 64
)

// hookLog is one pod's retained hook output. It is the io.WriteCloser the hook's stdout and stderr write to.
type hookLog struct {
	mu   sync.Mutex
	data []byte
}

// Write appends p, keeping only the last maxHookLogBytes.
func (l *hookLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.data = append(l.data, p...)
	if over := len(l.data) - maxHookLogBytes; over > 0 {
		l.data = append([]byte(nil), l.data[over:]...)
	}
	return len(p), nil
}

// Close is a no-op: the retained bytes stay readable.
func (l *hookLog) Close() error { return nil }

func (l *hookLog) snapshot(tail int) []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	data := l.data
	if tail > 0 {
		lines := 0
		for i := len(data) - 1; i >= 0; i-- {
			if data[i] == '\n' && i != len(data)-1 {
				lines++
				if lines == tail {
					data = data[i+1:]
					break
				}
			}
		}
	}
	return append([]byte(nil), data...)
}

// hookLogStore keeps the logs of the most recent pods. The zero value is ready to use.
type hookLogStore struct {
	mu    sync.Mutex
	order []string
	logs  map[string]*hookLog
}

// open returns the empty log for a pod about to run its hook, replacing any earlier one of the same name.
func (s *hookLogStore) open(namespace, name string) *hookLog {
	key := namespace + "/" + name
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logs == nil {
		s.logs = make(map[string]*hookLog)
	}
	if _, ok := s.logs[key]; ok {
		for i, k := range s.order {
			if k == key {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
	}
	l := &hookLog{}
	s.logs[key] = l
	s.order = append(s.order, key)
	for len(s.order) > maxHookLogPods {
		delete(s.logs, s.order[0])
		s.order = s.order[1:]
	}
	return l
}

func (s *hookLogStore) get(namespace, name string) (*hookLog, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.logs[namespace+"/"+name]
	return l, ok
}

// HookLogs returns the retained post-start hook output of a pod (its last tail lines when tail > 0), and whether
// the pod ran a hook under this provider.
func (c *MacOSClient) HookLogs(namespace, name string, tail int) (io.ReadCloser, bool) {
	l, ok := c.hookLogs.get(namespace, name)
	if !ok {
		return nil, false
	}
	return io.NopCloser(bytes.NewReader(l.snapshot(tail))), true
}
