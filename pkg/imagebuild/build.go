// Package imagebuild implements the build-image command: it derives a new macOS image from one
// the provider has already cached, by booting an APFS clone of its disk headless, running a
// reviewed script in the guest over SSH, stopping the VM from the host and streaming the result
// to the registry. It runs as root and is never reachable from a Pod or the Kubernetes API.
package imagebuild

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/internal/buildutil"
	"github.com/agoda-com/macOS-vz-kubelet/internal/envcfg"
	vzssh "github.com/agoda-com/macOS-vz-kubelet/internal/ssh"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/downloader"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/oci"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/resource"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/resourcemanager"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/vm"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/vm/config"

	"github.com/virtual-kubelet/virtual-kubelet/log"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

const (
	maxScriptSize = 1 << 20
	maxCredsSize  = 64 << 10
	maxConfigSize = 1 << 20
	// stopGrace lets the guest's page cache reach the virtual disk after `sync`.
	stopGrace = 3 * time.Second
	// maxDataDiskGiB bounds --data-disk-gib: the disk guard reserves a data disk's full logical
	// size on every node that pulls it, so an oversized disk is costly everywhere.
	maxDataDiskGiB = 256
)

// Options are the validated inputs of one build.
type Options struct {
	From, To   string // image references, both name:tag and under an allowed prefix
	ScriptPath string // absolute path of the provisioning script
	CredsPath  string // absolute path of the root-only registry credentials JSON
	CacheDir   string // provider cache root (holds blobs/ and build/)

	Prefixes       []string // VZ_ALLOWED_IMAGE_PREFIXES
	MinFreeBytes   uint64   // VZ_MIN_FREE_BYTES floor, 0 disables
	ExpectedGrowth uint64   // bytes the clone is expected to grow by
	OwnerUID       uint32   // required owner of the script, credentials and cached files (0 in production)

	// DataDiskGiB, when non-zero, builds a data disk instead of a bootable image: a blank sparse
	// disk of this size is attached to the build VM as a second writable device, the script
	// formats and fills it, and only that disk is pushed.
	DataDiskGiB uint64

	// AttachDisks are cached data disk images (name:tag, already pulled by the provider) attached
	// read-only to the build VM, so a recipe can read them (for example to copy files from an
	// existing Xcode disk onto the new one). Each must be a disk-only image under an allowed prefix.
	AttachDisks []string

	CPU              uint
	MemoryBytes      uint64
	NetworkInterface string
	ScriptTimeout    time.Duration
	// IPLookupTimeout overrides the VM's IP lookup timeout when non-zero (create-base's first boot).
	IPLookupTimeout time.Duration
	// CountVMUsers are extra users whose Virtualization VMs count as running macOS guests (for
	// example a user running tart). Root-owned VMs always count.
	CountVMUsers []string
}

// Seams replaced in tests.
var (
	// cloneFile makes a copy-on-write copy; src is not followed if it is a symlink.
	cloneFile = func(src, dst string) error { return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW) }
	// countVMProcesses counts the running macOS guests that take one of the 2 VM slots (see
	// countMacOSGuests); extraUsers are additional users whose VMs count.
	countVMProcesses = countVirtualizationProcesses
)

// Run performs the build. The build directory (the clone) is always deleted on return.
func Run(ctx context.Context, o Options) (err error) {
	logger := log.G(ctx)
	if err := buildutil.CheckRefs(o.From, o.To, o.Prefixes, envcfg.ImageAllowed); err != nil {
		return err
	}
	if _, err := vm.ValidateCPUCount(o.CPU); err != nil {
		return err
	}
	if _, err := vm.ValidateMemorySize(o.MemoryBytes); err != nil {
		return err
	}

	if len(o.AttachDisks) > 3 {
		return fmt.Errorf("at most 3 disks can be attached with --attach-data-disk")
	}
	for _, ref := range o.AttachDisks {
		if err := buildutil.CheckRefs(ref, o.To, o.Prefixes, envcfg.ImageAllowed); err != nil {
			return fmt.Errorf("--attach-data-disk: %w", err)
		}
	}
	if o.DataDiskGiB > maxDataDiskGiB {
		return fmt.Errorf("--data-disk-gib %d is above the maximum of %d", o.DataDiskGiB, maxDataDiskGiB)
	}

	script, err := buildutil.ReadFile(o.ScriptPath, buildutil.FilePolicy{MaxSize: maxScriptSize, OwnerUID: o.OwnerUID, ForbidBit: 0o022})
	if err != nil {
		return fmt.Errorf("script: %w", err)
	}
	credsRaw, err := buildutil.ReadFile(o.CredsPath, buildutil.FilePolicy{MaxSize: maxCredsSize, OwnerUID: o.OwnerUID, ForbidBit: 0o077})
	if err != nil {
		return fmt.Errorf("credentials file: %w", err)
	}
	creds, err := buildutil.ParseCredentials(credsRaw)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(script)
	scriptSHA := hex.EncodeToString(sum[:])

	// Concurrency with the provider: Virtualization.framework allows 2 macOS VMs on the whole host
	// and the provider has no endpoint to reserve a slot from another process. The simplest safe
	// rule is to require that no macOS guest is running, so both slots are free for this build.
	// Linux VMs (Colima) do not count against the cap.
	if n, err := countVMProcesses(o.CountVMUsers); err != nil {
		return fmt.Errorf("could not count running virtual machines: %w", err)
	} else if n > 0 {
		return fmt.Errorf("%d macOS virtual machine(s) are running on this host; stop or cordon them first (the host allows 2 and the build needs one)", n)
	}

	free, err := downloader.FreeBytes(o.CacheDir)
	if err != nil {
		return fmt.Errorf("failed to read free disk space: %w", err)
	}
	needed := neededBytes(o)
	if !downloader.Fits(free, needed, o.MinFreeBytes) {
		return fmt.Errorf("%w: %d bytes free, build expects %d of growth (clone plus data disk), floor %d", downloader.ErrInsufficientSpace, free, needed, o.MinFreeBytes)
	}

	img, err := downloader.VerifyCached(ctx, o.CacheDir, o.From, resource.RegistryCredentials{
		Username: creds.Username, Password: creds.Password, IdentityToken: creds.IdentityToken,
	})
	if err != nil {
		return err
	}
	logger.Infof("Base image %s is cached and verified (manifest %s)", o.From, img.ManifestDigest)

	bd, err := buildutil.NewBuildDir(filepath.Join(o.CacheDir, "build"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, bd.Close()) }() // runs last: after the VM is stopped

	// Abort and clean up if free space crosses the floor at any point (clone growth, push).
	ctx, stopWatch := downloader.WatchFreeSpace(ctx, o.CacheDir, o.MinFreeBytes)
	defer stopWatch()
	defer func() {
		if cause := context.Cause(ctx); err != nil && errors.Is(cause, downloader.ErrInsufficientSpace) {
			err = errors.Join(err, cause)
		}
	}()

	if err := cloneInputs(img.Dir, bd, o.OwnerUID); err != nil {
		return err
	}
	diskPath, _ := bd.Path("disk.img")
	auxPath, _ := bd.Path("aux.img")
	cfgPath, _ := bd.Path("config.json")

	cfgBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	var imgCfg oci.Config
	if err := json.Unmarshal(cfgBytes, &imgCfg); err != nil {
		return fmt.Errorf("base config.json: %w", err)
	}

	var disks []config.Disk
	for _, ref := range o.AttachDisks {
		d, err := downloader.VerifyCached(ctx, o.CacheDir, ref, resource.RegistryCredentials{
			Username: creds.Username, Password: creds.Password, IdentityToken: creds.IdentityToken,
		})
		if err != nil {
			return fmt.Errorf("attached disk %s must be pulled first (run a Pod with it): %w", ref, err)
		}
		src := filepath.Join(d.Dir, "disk.img")
		fi, err := os.Lstat(src)
		if err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("attached disk %s: cached disk.img is missing or not a regular file", ref)
		}
		if _, err := os.Lstat(filepath.Join(d.Dir, "aux.img")); err == nil {
			return fmt.Errorf("attached disk %s is a bootable image, not a data disk", ref)
		}
		disks = append(disks, config.Disk{Path: src, ReadOnly: true})
	}
	dataPath := ""
	if o.DataDiskGiB > 0 {
		dataPath, _ = bd.Path("data.img")
		if err := createSparseDisk(dataPath, o.DataDiskGiB); err != nil {
			return err
		}
		disks = append(disks, config.Disk{Path: dataPath})
	}

	instance, err := bootClone(ctx, o, config.MacPlatformConfigurationOptions{
		BlockStoragePath:      diskPath,
		AuxiliaryStoragePath:  auxPath,
		HardwareModelData:     imgCfg.HardwareModelData,
		MachineIdentifierData: imgCfg.MachineIdData,
	}, disks)
	if err != nil {
		return err
	}
	// Always stop from the host: a guest shutdown leaks a Virtualization.framework VM slot on
	// macOS 26.5. The context is detached so cleanup still happens when ctx was cancelled.
	stopped := false
	defer func() {
		if !stopped {
			_ = instance.Stop(context.WithoutCancel(ctx))
		}
	}()

	if err := provision(ctx, instance.IPAddress, script, o.ScriptTimeout, o.DataDiskGiB<<30); err != nil {
		return err
	}
	time.Sleep(stopGrace)
	if err := instance.Stop(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("failed to stop the build VM: %w", err)
	}
	stopped = true
	logger.Info("Build VM stopped from the host; pushing")

	var baseDiskDigest string
	for _, l := range img.Layers {
		if l.MediaType == string(oci.MediaTypeDiskImage) {
			baseDiskDigest = l.Annotations[oci.AnnotationUncompressedDigest]
		}
	}
	push := buildutil.PushOptions{
		Ref:    o.To,
		Creds:  creds,
		Disk:   diskPath,
		Aux:    auxPath,
		Config: cfgPath,
		Annotations: map[string]string{
			"com.agoda.macosvz.build.base-ref":             o.From,
			"com.agoda.macosvz.build.base-manifest-digest": img.ManifestDigest,
			"com.agoda.macosvz.build.base-disk-digest":     baseDiskDigest,
			"com.agoda.macosvz.build.script-sha256":        scriptSHA,
			"com.agoda.macosvz.build.machine-identifier":   "shared with the base image",
		},
	}
	if dataPath != "" {
		// A data disk is the blank disk the script filled; the boot clone is discarded.
		push.Disk, push.Aux, push.Config = dataPath, "", ""
	}
	res, err := buildutil.Push(ctx, push)
	if err != nil {
		return err
	}
	logger.Infof("Pushed %s (manifest %s)", o.To, res.ManifestDigest)
	return nil
}

// neededBytes is the disk growth a build is expected to cause; the sparse data disk is filled by
// the script, so it counts in full.
func neededBytes(o Options) uint64 { return o.ExpectedGrowth + o.DataDiskGiB<<30 }

// createSparseDisk creates an empty raw disk image of gib GiB that takes no space until written.
func createSparseDisk(path string, gib uint64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Truncate(path, int64(gib<<30))
}

// cloneInputs APFS-clones disk.img and aux.img from the cached image into the build
// directory and copies config.json. File names are fixed; the sources are the cache's own
// files, opened without following symlinks and required to be regular files owned by ownerUID.
func cloneInputs(imageDir string, bd *buildutil.BuildDir, ownerUID uint32) error {
	for _, name := range []string{"disk.img", "aux.img"} {
		src := filepath.Join(imageDir, name)
		fi, err := os.Lstat(src)
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("cached %s is not a regular file", src)
		}
		dst, err := bd.Path(name)
		if err != nil {
			return err
		}
		if err := cloneFile(src, dst); err != nil {
			return fmt.Errorf("failed to clone %s (the build dir must be on the cache volume): %w", name, err)
		}
	}
	cfg, err := buildutil.ReadFile(filepath.Join(imageDir, "config.json"), buildutil.FilePolicy{MaxSize: maxConfigSize, OwnerUID: ownerUID, ForbidBit: 0o022})
	if err != nil {
		return fmt.Errorf("cached config.json: %w", err)
	}
	dst, _ := bd.Path("config.json")
	return os.WriteFile(dst, cfg, 0o600)
}

// bootClone starts a headless VM from the cloned disk (no overlay: the clone is the disk).
func bootClone(ctx context.Context, o Options, cfg config.MacPlatformConfigurationOptions, disks []config.Disk) (*vm.VirtualMachineInstance, error) {
	platform, err := config.NewPlatformConfiguration(ctx, cfg, false, "build-image")
	if err != nil {
		return nil, fmt.Errorf("failed to create platform configuration: %w", err)
	}
	vmCfg, err := config.NewVirtualMachineConfiguration(ctx, platform, o.CPU, o.MemoryBytes, o.NetworkInterface, nil, disks)
	if err != nil {
		return nil, fmt.Errorf("failed to create virtual machine configuration: %w", err)
	}
	instance, err := vm.NewVirtualMachineInstance(ctx, vmCfg)
	if err != nil {
		return nil, err
	}
	instance.IPLookupTimeout = o.IPLookupTimeout
	if err := instance.Start(ctx); err != nil {
		_ = instance.Stop(context.WithoutCancel(ctx))
		return nil, err
	}
	return instance, nil
}

// provision waits for sshd, copies the script into the guest, runs it as the SSH user and
// syncs. A non-zero script exit is an error, so nothing is pushed.
func provision(ctx context.Context, ip string, script []byte, timeout time.Duration, dataDiskBytes uint64) error {
	sshCfg, err := resourcemanager.SSHClientConfig()
	if err != nil {
		return err
	}
	client, err := waitForSettledSSH(ctx, ip, sshCfg, envcfg.GuestSettle())
	if err != nil {
		return err
	}
	defer client.Close()

	if err := runRemote(ctx, client, `umask 077 && mkdir -p "$HOME/.build" && cat > "$HOME/.build/script.sh"`, bytes.NewReader(script), nil, time.Minute); err != nil {
		return fmt.Errorf("failed to copy the script into the guest: %w", err)
	}
	log.G(ctx).Info("Running the provisioning script in the guest")
	if err := runRemote(ctx, client, `/bin/bash "$HOME/.build/script.sh"`, nil, os.Stderr, timeout); err != nil {
		return fmt.Errorf("provisioning script: %w", err)
	}
	if dataDiskBytes > 0 {
		// The host stop below is a hard stop: unmount the data disk first so its APFS container is clean.
		if err := runRemote(ctx, client, unmountCommand(dataDiskBytes), strings.NewReader(unmountDataDiskScript), os.Stderr, 5*time.Minute); err != nil {
			return fmt.Errorf("failed to unmount the data disk (the script must leave it with no open files): %w", err)
		}
	}
	// Remove the script from the image, then flush guest writes to the virtual disk.
	if err := runRemote(ctx, client, `rm -rf "$HOME/.build"; sync`, nil, os.Stderr, 5*time.Minute); err != nil {
		return fmt.Errorf("failed to sync the guest: %w", err)
	}
	return nil
}

// unmountDataDiskScript finds the data disk and unmounts it. The data disk is the only physical
// whole disk whose size is exactly $1 bytes (what the host created); none or several matches fail,
// so the boot disk can never be picked by mistake.
const unmountDataDiskScript = `set -eu
want="$1"
n=$(diskutil list -plist physical | plutil -extract WholeDisks raw -o - -)
found=""
i=0
while [ "$i" -lt "$n" ]; do
  d=$(diskutil list -plist physical | plutil -extract "WholeDisks.$i" raw -o - -)
  size=$(diskutil info -plist "$d" | plutil -extract TotalSize raw -o - -)
  if [ "$size" = "$want" ]; then found="$found $d"; fi
  i=$((i + 1))
done
set -- $found
if [ "$#" -ne 1 ]; then echo "expected one physical disk of $want bytes, found: ${found:-none}" >&2; exit 1; fi
sync
sudo -n diskutil unmountDisk force "/dev/$1"
`

// unmountCommand runs unmountDataDiskScript (on stdin) for a data disk of size bytes.
func unmountCommand(size uint64) string { return fmt.Sprintf("/bin/bash -s -- %d", size) }

// guestUptimeCommand prints the guest's whole seconds since boot.
const guestUptimeCommand = `echo $(( $(date +%s) - $(sysctl -n kern.boottime | sed -E 's/.*sec = ([0-9]+).*/\1/') ))`

// waitForSettledSSH returns a client dialled once the guest has been up for settle. sshd answers
// before a macOS guest's boot settles, and early boot tears down every live session
// (envcfg.GuestSettle), so a young guest's connection is closed and re-dialled after the wait.
func waitForSettledSSH(ctx context.Context, ip string, cfg *ssh.ClientConfig, settle time.Duration) (*ssh.Client, error) {
	deadline := time.Now().Add(settle + envcfg.SSHReadinessTimeout())
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("guest boot did not settle: no session survived within %s", settle+envcfg.SSHReadinessTimeout())
		}
		client, err := waitForSSH(ctx, ip, cfg, envcfg.SSHReadinessTimeout())
		if err != nil {
			return nil, err
		}
		var out bytes.Buffer
		wait := 2 * time.Second // the probe's session was torn down: early boot, retry shortly
		if err := runRemote(ctx, client, guestUptimeCommand, nil, &out, time.Minute); err == nil {
			up, perr := parseUptime(out.String())
			if perr != nil {
				_ = client.Close()
				return nil, perr
			}
			if up >= settle {
				return client, nil
			}
			wait = settle - up
		}
		_ = client.Close()
		log.G(ctx).Infof("Waiting %s for the guest's boot to settle before provisioning", wait.Round(time.Second))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// parseUptime reads guestUptimeCommand's output.
func parseUptime(s string) (time.Duration, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("guest uptime: unexpected output %q", strings.TrimSpace(s))
	}
	return time.Duration(n) * time.Second, nil
}

func waitForSSH(ctx context.Context, ip string, cfg *ssh.ClientConfig, budget time.Duration) (*ssh.Client, error) {
	deadline := time.Now().Add(budget)
	var last error
	for {
		c, err := vzssh.DialContext(ctx, "tcp", ip+":22", cfg)
		if err == nil {
			return c, nil
		}
		last = err
		if strings.Contains(err.Error(), "unable to authenticate") {
			return nil, err // permanent
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("sshd not reachable after %s: %w", budget, last)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// runRemote runs cmd in a new session with a deadline; cancelling closes the session.
func runRemote(ctx context.Context, client *ssh.Client, cmd string, stdin io.Reader, out io.Writer, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdin = stdin
	sess.Stdout, sess.Stderr = out, out
	stop := context.AfterFunc(ctx, func() { _ = sess.Close() })
	defer stop()

	err = sess.Run(cmd)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		return fmt.Errorf("exit status %d", exit.ExitStatus())
	}
	var missing *ssh.ExitMissingError
	if errors.As(err, &missing) {
		return fmt.Errorf("the guest closed the SSH session before the command exited (its sshd-session was torn down: early boot, an sshd restart or a guest reboot), not a script failure: %w", err)
	}
	return err
}

// vmProcessMarker is in the command line of every Virtualization.framework VM process.
const vmProcessMarker = "com.apple.Virtualization.VirtualMachine"

// countVirtualizationProcesses lists processes with ps and counts the macOS guests among them.
// Any failure to enumerate is an error, so the caller fails closed.
func countVirtualizationProcesses(extraUsers []string) (int, error) {
	uids := map[int]bool{0: true}
	for _, name := range extraUsers {
		u, err := user.Lookup(name)
		if err != nil {
			return 0, fmt.Errorf("--count-vm-user %q: %w", name, err)
		}
		uid, err := strconv.Atoi(u.Uid)
		if err != nil {
			return 0, err
		}
		uids[uid] = true
	}
	out, err := exec.Command("/bin/ps", "-axo", "pid=,uid=,command=").Output()
	if err != nil {
		return 0, fmt.Errorf("ps: %w", err)
	}
	return countMacOSGuests(string(out), uids), nil
}

// countMacOSGuests counts Virtualization VM processes in ps output ("pid uid command" lines)
// owned by one of uids. Apple's 2-VM cap applies to macOS guests only, and a process does not
// say what it runs, so ownership is the signal: the provider runs as root and starts every
// macOS VM of this node, while Colima's Linux VM belongs to its own user and is not counted.
// A VM process whose line cannot be parsed is counted (fail closed).
func countMacOSGuests(ps string, uids map[int]bool) int {
	n := 0
	for _, line := range strings.Split(ps, "\n") {
		if !strings.Contains(line, vmProcessMarker) {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 3 {
			n++
			continue
		}
		uid, err := strconv.Atoi(f[1])
		if err != nil || uids[uid] {
			n++
		}
	}
	return n
}
