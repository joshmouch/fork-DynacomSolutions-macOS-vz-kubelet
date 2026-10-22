package imagebuild

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/internal/buildutil"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/downloader"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testFrom = "registry.example.com/macos-vz/base:1"
	testTo   = "registry.example.com/macos-vz/base:2"
)

func copyClone(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

func testOptions(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "script.sh")
	require.NoError(t, os.WriteFile(script, []byte("echo hi\n"), 0o600))
	creds := filepath.Join(dir, "creds.json")
	require.NoError(t, os.WriteFile(creds, []byte(`{"username":"u","password":"p"}`), 0o600))
	return Options{
		From: testFrom, To: testTo, ScriptPath: script, CredsPath: creds,
		CacheDir: filepath.Join(dir, "cache"), Prefixes: []string{"registry.example.com/macos-vz/"},
		MinFreeBytes: 1, OwnerUID: uint32(os.Getuid()), CPU: 2, MemoryBytes: 4 << 30,
	}
}

func TestCloneInputsClonesIntoBuildDirAndCleansUp(t *testing.T) {
	old := cloneFile
	cloneFile = copyClone
	defer func() { cloneFile = old }()

	imageDir := t.TempDir()
	for n, c := range map[string]string{"disk.img": "disk", "aux.img": "aux", "config.json": `{"os":"darwin"}`} {
		require.NoError(t, os.WriteFile(filepath.Join(imageDir, n), []byte(c), 0o600))
	}
	root := filepath.Join(t.TempDir(), "build")
	bd, err := buildutil.NewBuildDir(root)
	require.NoError(t, err)

	require.NoError(t, cloneInputs(imageDir, bd, uint32(os.Getuid())))
	for n, c := range map[string]string{"disk.img": "disk", "aux.img": "aux", "config.json": `{"os":"darwin"}`} {
		p, _ := bd.Path(n)
		got, err := os.ReadFile(p)
		require.NoError(t, err)
		assert.Equal(t, c, string(got))
	}

	require.NoError(t, bd.Close())
	assert.NoDirExists(t, bd.Dir(), "the clone is deleted")
}

func TestCloneInputsRefusesSymlinkedDisk(t *testing.T) {
	old := cloneFile
	cloneFile = copyClone
	defer func() { cloneFile = old }()

	imageDir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(secret, []byte("s"), 0o600))
	require.NoError(t, os.Symlink(secret, filepath.Join(imageDir, "disk.img")))
	require.NoError(t, os.WriteFile(filepath.Join(imageDir, "aux.img"), []byte("a"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(imageDir, "config.json"), []byte("{}"), 0o600))

	bd, err := buildutil.NewBuildDir(filepath.Join(t.TempDir(), "build"))
	require.NoError(t, err)
	defer bd.Close()
	assert.ErrorContains(t, cloneInputs(imageDir, bd, uint32(os.Getuid())), "not a regular file")
}

func TestCountMacOSGuests(t *testing.T) {
	const vz = "/System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/com.apple.Virtualization.VirtualMachine.xpc/Contents/MacOS/com.apple.Virtualization.VirtualMachine"
	ps := "  100     0 /sbin/launchd\n" +
		"27174   501 " + vz + "\n" + // Colima's Linux VM under macnode: not counted
		" 4001     0 " + vz + "\n" + // the provider's macOS VM
		" 4002     0 " + vz + "\n" +
		" 5000   502 " + vz + "\n" + // another user's VM
		"  junk " + vz + "\n" + // unparseable VM line: counted
		"  200     0 /usr/bin/something-else\n"
	root := map[int]bool{0: true}
	assert.Equal(t, 3, countMacOSGuests(ps, root), "root VMs plus the unparseable line")
	assert.Equal(t, 4, countMacOSGuests(ps, map[int]bool{0: true, 502: true}), "an extra user's VMs count when named")
	assert.Equal(t, 0, countMacOSGuests("27174 501 "+vz, map[int]bool{}), "a uid outside the set does not count")
	assert.Equal(t, 0, countMacOSGuests("", root))
	assert.Equal(t, 0, countMacOSGuests(" 27174   501 "+vz+"\n", root), "Colima alone does not block a build")
}

func TestCountVirtualizationProcessesRejectsUnknownUser(t *testing.T) {
	_, err := countVirtualizationProcesses([]string{"no-such-user-xyz"})
	assert.Error(t, err)
}

func TestRunRefusals(t *testing.T) {
	oldCount := countVMProcesses
	defer func() { countVMProcesses = oldCount }()
	countVMProcesses = func([]string) (int, error) { return 0, nil }

	t.Run("disallowed destination", func(t *testing.T) {
		o := testOptions(t)
		o.To = "registry.example.com/other/base:2"
		assert.ErrorContains(t, Run(context.Background(), o), "not allowed")
	})

	t.Run("a running VM", func(t *testing.T) {
		o := testOptions(t)
		countVMProcesses = func([]string) (int, error) { return 1, nil }
		defer func() { countVMProcesses = func([]string) (int, error) { return 0, nil } }()
		assert.ErrorContains(t, Run(context.Background(), o), "macOS virtual machine(s) are running")
		assert.NoDirExists(t, filepath.Join(o.CacheDir, "build"))
	})

	t.Run("not enough free space", func(t *testing.T) {
		o := testOptions(t)
		require.NoError(t, os.MkdirAll(o.CacheDir, 0o700))
		o.MinFreeBytes = 1 << 62
		err := Run(context.Background(), o)
		assert.True(t, errors.Is(err, downloader.ErrInsufficientSpace), "got %v", err)
		assert.NoDirExists(t, filepath.Join(o.CacheDir, "build"))
	})

	t.Run("data disk too large", func(t *testing.T) {
		o := testOptions(t)
		o.DataDiskGiB = maxDataDiskGiB + 1
		assert.ErrorContains(t, Run(context.Background(), o), "--data-disk-gib")
	})

	t.Run("attached disk not allowed", func(t *testing.T) {
		o := testOptions(t)
		o.AttachDisks = []string{"registry.example.com/other/disk:1"}
		assert.ErrorContains(t, Run(context.Background(), o), "--attach-data-disk")
	})

	t.Run("too many attached disks", func(t *testing.T) {
		o := testOptions(t)
		o.AttachDisks = []string{"a:1", "b:1", "c:1", "d:1"}
		assert.ErrorContains(t, Run(context.Background(), o), "at most 3")
	})

	t.Run("script writable by others", func(t *testing.T) {
		o := testOptions(t)
		require.NoError(t, os.Chmod(o.ScriptPath, 0o666))
		assert.ErrorContains(t, Run(context.Background(), o), "too permissive")
	})

	t.Run("credentials readable by others", func(t *testing.T) {
		o := testOptions(t)
		require.NoError(t, os.Chmod(o.CredsPath, 0o644))
		assert.ErrorContains(t, Run(context.Background(), o), "too permissive")
	})
}

func TestUnmountCommand(t *testing.T) {
	assert.Equal(t, "/bin/bash -s -- 8589934592", unmountCommand(8<<30))
	assert.Contains(t, unmountDataDiskScript, `sudo -n diskutil unmountDisk force "/dev/$1"`)
}

func TestNeededBytesCountsTheDataDisk(t *testing.T) {
	assert.Equal(t, uint64(10<<30), neededBytes(Options{ExpectedGrowth: 10 << 30}))
	assert.Equal(t, uint64(10<<30+64<<30), neededBytes(Options{ExpectedGrowth: 10 << 30, DataDiskGiB: 64}))
}

func TestCreateSparseDisk(t *testing.T) {
	p := filepath.Join(t.TempDir(), "data.img")
	require.NoError(t, createSparseDisk(p, 2))
	fi, err := os.Stat(p)
	require.NoError(t, err)
	assert.Equal(t, int64(2<<30), fi.Size())
	assert.Error(t, createSparseDisk(p, 2), "an existing file is not reused")
}

// parseUptime reads the guest's whole seconds since boot and refuses anything else, so a
// garbled probe fails the build instead of being read as a settled guest.
func TestParseUptime(t *testing.T) {
	up, err := parseUptime("18\n")
	require.NoError(t, err)
	assert.Equal(t, 18*time.Second, up)
	for _, bad := range []string{"", "abc", "-3", "{ sec = 1 }"} {
		_, err := parseUptime(bad)
		assert.Error(t, err, bad)
	}
}
