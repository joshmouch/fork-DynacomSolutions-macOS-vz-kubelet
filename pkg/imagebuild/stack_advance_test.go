package imagebuild

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAdvanceStackBuildBudgetAndRejoin(t *testing.T) {
	previous := runStackLayer
	defer func() { runStackLayer = previous }()
	cached := map[string]bool{}
	built := 0
	runStackLayer = func(_ context.Context, o Options) error {
		if cached[o.InputIdentity] {
			*o.ReuseResult = true
			return nil
		}
		if o.ReuseOnly {
			return errBuildRequired
		}
		cached[o.InputIdentity] = true
		built++
		return nil
	}
	layers := []StackLayer{{Name: "sdk"}, {Name: "node"}}
	ids := []string{"sdk-input", "node-input"}
	o := Options{From: "base@sha256:immutable"}
	first, err := advanceStackLayers(context.Background(), o, "registry/runner", layers, ids, 1)
	require.NoError(t, err)
	require.Equal(t, StackResult{State: "pending", Ready: 1, Total: 2, Ref: "registry/runner:sdk-input", InputIdentity: "sdk-input"}, first)
	require.Equal(t, 1, built)
	second, err := advanceStackLayers(context.Background(), o, "registry/runner", layers, ids, 1)
	require.NoError(t, err)
	require.Equal(t, "complete", second.State)
	require.Equal(t, 2, second.Ready)
	require.Equal(t, "node-input", second.InputIdentity)
	require.Equal(t, 2, built, "qualified lower prefix must not consume the next invocation's build budget")
	noop, err := advanceStackLayers(context.Background(), o, "registry/runner", layers, ids, 1)
	require.NoError(t, err)
	require.Equal(t, second, noop)
	require.Equal(t, 2, built)
	// A qualified upper prefix remains reusable after the same lower identity is rebuilt.
	delete(cached, "sdk-input")
	_, err = advanceStackLayers(context.Background(), o, "registry/runner", layers, ids, 1)
	require.NoError(t, err)
	require.Equal(t, 3, built)
}

func TestAdvanceStackNeverProjectsConstructionFailureAsPending(t *testing.T) {
	previous := runStackLayer
	defer func() { runStackLayer = previous }()
	failure := errors.New("invalid sealed image")
	runStackLayer = func(context.Context, Options) error { return failure }
	result, err := advanceStackLayers(context.Background(), Options{From: "base"}, "registry/runner", []StackLayer{{Name: "sdk"}}, []string{"identity"}, 1)
	require.ErrorIs(t, err, failure)
	require.Empty(t, result.State)
}

func TestReuseOnlyStopsBeforeVMAdmission(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "component.sh")
	require.NoError(t, os.WriteFile(script, []byte("exit 0\n"), 0600))
	previous := countVMProcesses
	defer func() { countVMProcesses = previous }()
	countVMProcesses = func([]string) (int, error) { t.Fatal("reuse-only admission started VM preparation"); return 0, nil }
	err := Run(context.Background(), Options{
		From: "registry.example/base:1", To: "registry.example/runner:immutable",
		Prefixes: []string{"registry.example/"}, ScriptPath: script,
		CacheDir: root, OwnerUID: uint32(os.Geteuid()), Local: true,
		InputIdentity: "requested-prefix", CPU: 4, MemoryBytes: 8 << 30, ReuseOnly: true,
	})
	require.ErrorIs(t, err, errBuildRequired)
}

func TestPendingDoesNotHideCustodyCleanupFailure(t *testing.T) {
	require.True(t, onlyBuildRequired(errors.Join(errBuildRequired, nil)))
	failure := errors.New("custody cleanup failed")
	require.False(t, onlyBuildRequired(errors.Join(errBuildRequired, failure)))
	previous := runStackLayer
	defer func() { runStackLayer = previous }()
	runStackLayer = func(_ context.Context, o Options) error {
		if o.ReuseOnly {
			return errors.Join(errBuildRequired, failure)
		}
		return nil
	}
	result, err := advanceStackLayers(context.Background(), Options{From: "base"}, "registry/runner", []StackLayer{{Name: "sdk"}, {Name: "node"}}, []string{"sdk", "node"}, 1)
	require.ErrorIs(t, err, failure)
	require.Empty(t, result.State)
}
