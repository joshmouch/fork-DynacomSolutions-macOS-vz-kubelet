package resourcemanager

import (
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHookLogsServeOutputAfterTheHookEnds(t *testing.T) {
	c := &MacOSClient{}
	out := c.hookLogs.open("ns", "pod")
	_, _ = out.Write([]byte("one\ntwo\nthree\n"))
	_ = out.Close()

	all, ok := c.HookLogs("ns", "pod", 0)
	require.True(t, ok)
	b, _ := io.ReadAll(all)
	assert.Equal(t, "one\ntwo\nthree\n", string(b))

	tail, _ := c.HookLogs("ns", "pod", 2)
	b, _ = io.ReadAll(tail)
	assert.Equal(t, "two\nthree\n", string(b))

	_, ok = c.HookLogs("ns", "other", 0)
	assert.False(t, ok)
}

func TestHookLogsAreBounded(t *testing.T) {
	c := &MacOSClient{}
	out := c.hookLogs.open("ns", "pod")
	_, _ = out.Write(make([]byte, maxHookLogBytes+10))
	assert.Len(t, out.data, maxHookLogBytes)
	for i := 0; i < maxHookLogPods+1; i++ {
		c.hookLogs.open("ns", string(rune('a'+i%26))+string(rune('A'+i/26)))
	}
	_, ok := c.HookLogs("ns", "pod", 0)
	assert.False(t, ok, "the oldest pod's log is dropped past maxHookLogPods")
}
