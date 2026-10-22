package envcfg_test

import (
	"testing"

	"github.com/agoda-com/macOS-vz-kubelet/internal/envcfg"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SSHJump: unset or blank dials guests directly; user@host:port is parsed; anything else is an
// error rather than a silent direct dial.
func TestSSHJump(t *testing.T) {
	t.Setenv("VZ_SSH_JUMP", "")
	_, _, ok, err := envcfg.SSHJump()
	require.NoError(t, err)
	assert.False(t, ok)

	t.Setenv("VZ_SSH_JUMP", " josh@127.0.0.1:22 ")
	user, addr, ok, err := envcfg.SSHJump()
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "josh", user)
	assert.Equal(t, "127.0.0.1:22", addr)

	for _, bad := range []string{"127.0.0.1:22", "@127.0.0.1:22", "josh@127.0.0.1", "josh@"} {
		t.Setenv("VZ_SSH_JUMP", bad)
		_, _, _, err := envcfg.SSHJump()
		assert.Error(t, err, bad)
	}
}
