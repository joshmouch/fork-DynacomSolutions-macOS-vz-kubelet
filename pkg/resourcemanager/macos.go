package resourcemanager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	vmdata "github.com/agoda-com/macOS-vz-kubelet/internal/data/vm"
	"github.com/agoda-com/macOS-vz-kubelet/internal/execerror"
	"github.com/agoda-com/macOS-vz-kubelet/internal/execwindow"
	"github.com/agoda-com/macOS-vz-kubelet/internal/sshconn"
	"github.com/agoda-com/macOS-vz-kubelet/internal/vmcap"
	"github.com/agoda-com/macOS-vz-kubelet/internal/volumes"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/downloader"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/event"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/resource"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/vm"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/vm/config"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/trace"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// MaxVirtualMachines is the default (licensed) number of virtual machines that can be created.
	// This is a kernel level limitation by Apple and is enforced within Virtualization.framework.
	// The effective cap is vmcap.Configured() (VZ_MAX_MACOS_VMS).
	MaxVirtualMachines = vmcap.Default
)

// VirtualMachineParams encapsulates the parameters required for creating a virtual machine.
type VirtualMachineParams struct {
	UID             string
	Image           string
	Namespace       string
	Name            string
	ContainerName   string
	CPU             uint
	MemorySize      uint64
	Mounts          []volumes.Mount
	Env             []corev1.EnvVar
	PostStartAction *resource.ExecAction
	// StopOnPostStart stops the VM from the host once the postStart hook exits and reports the
	// hook's exit code (see resource.StopWhenPostStartExitsAnnotation).
	StopOnPostStart  bool
	IgnoreImageCache bool
	RegistryCreds    resource.RegistryCredentials
	// DataDisks are attached read-only after the boot disk, in order (see resource.DataDisksAnnotation).
	DataDisks []DataDisk
}

// DataDisk is a data disk image to pull and attach.
type DataDisk struct {
	Ref           string
	RegistryCreds resource.RegistryCredentials
}

// MacOSClient manages the lifecycle of macOS virtual machines.
type MacOSClient struct {
	downloadManager *downloader.Manager
	vms             vmdata.VirtualMachineData

	eventRecorder              event.EventRecorder
	networkInterfaceIdentifier string
	vmPermits                  chan struct{}
	hookLogs                   hookLogStore
	// executions writes one bounded-execution window per VM; nil unless the environment turns it on (execwindow.FromEnv).
	executions *execwindow.Recorder
}

// NewMacOSClient initializes a new MacOSClient instance.
func NewMacOSClient(ctx context.Context, eventRecorder event.EventRecorder, networkInterfaceIdentifier, cachePath string) *MacOSClient {
	ctx, span := trace.StartSpan(ctx, "MacOSClient.NewMacOSClient")
	_ = span.WithFields(ctx, log.Fields{
		"networkInterfaceIdentifier": networkInterfaceIdentifier,
		"cachePath":                  cachePath,
	})
	defer span.End()

	c := &MacOSClient{
		eventRecorder:              eventRecorder,
		networkInterfaceIdentifier: networkInterfaceIdentifier,
		downloadManager:            downloader.NewManager(eventRecorder, cachePath),
		vmPermits:                  make(chan struct{}, vmcap.Configured()),
		vms:                        vmdata.NewVirtualMachineData(),
		executions:                 execwindow.FromEnv(),
	}
	// Images referenced by any Pod/VM on this node must never be evicted.
	c.downloadManager.SetInUse(func() (refs []string) {
		for _, info := range c.vms.All() {
			refs = append(refs, info.Ref)
			refs = append(refs, info.DataDiskRefs...)
		}
		return refs
	})
	return c
}

// CreateVirtualMachine creates a new virtual machine with the specified parameters.
func (c *MacOSClient) CreateVirtualMachine(ctx context.Context, params VirtualMachineParams) (err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSClient.CreateVirtualMachine")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	_, loaded := c.vms.LoadOrStore(params.Namespace, params.Name, vmdata.VirtualMachineInfo{
		Ref:          params.Image,
		DataDiskRefs: dataDiskRefs(params.DataDisks),
		Resource:     resource.NewMacOSVirtualMachine(params.Env),
		// Persistent SSH connection shared across every exec for this VM; dials
		// lazily on first NewSession. Closed in DeleteVirtualMachine.
		SSHConn: sshconn.New(c.sshDialFunc(params.Namespace, params.Name)),
	})
	if loaded {
		return errdefs.AsInvalidInput(fmt.Errorf("virtual machine already exists"))
	}

	c.eventRecorder.PullingImage(ctx, params.Image, params.ContainerName)
	for _, d := range params.DataDisks {
		c.eventRecorder.PullingImage(ctx, d.Ref, params.ContainerName)
	}

	// Start the asynchronous creation of the virtual machine
	go c.handleVirtualMachineCreation(ctx, params)

	return nil
}

func (c *MacOSClient) handleVirtualMachineCreation(ctx context.Context, params VirtualMachineParams) {
	var err error
	ctx, span := trace.StartSpan(ctx, "MacOSClient.handleVirtualMachineCreation")
	defer func() {
		c.finalizeVirtualMachineInfo(ctx, params, err)
		span.SetStatus(err)
		span.End()
	}()
	logger := log.G(ctx)
	logger.Debugf("Creating virtual machine with params: %+v", params)

	// Manage download
	downloadCtx, cancel := context.WithCancel(ctx) // create a new context to manage the download
	defer cancel()
	_, found := c.vms.Update(params.Namespace, params.Name, func(i vmdata.VirtualMachineInfo) vmdata.VirtualMachineInfo {
		i.DownloadCancelFunc = cancel
		return i
	})
	if !found {
		logger.Debug("virtual machine info expired")
		return
	}

	// The image and the data disks download in parallel; the first failure cancels the rest.
	var cfg config.MacPlatformConfigurationOptions
	disks := make([]config.Disk, len(params.DataDisks))
	g, gctx := errgroup.WithContext(downloadCtx)
	g.Go(func() error {
		var duration time.Duration
		var err error
		cfg, duration, err = c.downloadManager.Download(gctx, params.Image, params.IgnoreImageCache, params.RegistryCreds)
		c.reportPull(ctx, params.Image, params.ContainerName, duration, err)
		return err
	})
	for i, d := range params.DataDisks {
		g.Go(func() error {
			diskCfg, duration, err := c.downloadManager.Download(gctx, d.Ref, false, d.RegistryCreds)
			c.reportPull(ctx, d.Ref, params.ContainerName, duration, err)
			// The cached file is shared by every VM that uses it, so it is attached read-only.
			disks[i] = config.Disk{Path: diskCfg.BlockStoragePath, ReadOnly: true}
			return err
		})
	}
	if err = g.Wait(); err != nil {
		return
	}

	logger.Debug(cfg)

	// A disk-only image is a data disk, not something to boot.
	if cfg.AuxiliaryStoragePath == "" {
		err = errdefs.AsInvalidInput(errors.New("image is a data disk, not a bootable VM image"))
		c.eventRecorder.FailedToCreateContainer(ctx, params.ContainerName, err)
		return
	}

	// Wait until resources are available to proceed with the virtual machine creation
	guard, acquireErr := c.acquirePermit(ctx, types.NamespacedName{Namespace: params.Namespace, Name: params.Name})
	if acquireErr != nil {
		err = acquireErr
		return
	}
	defer guard.Release(ctx)

	// The VM's host process is the one that appears from here on; its counters begin with the VM.
	hostProcessesBefore := execwindow.SnapshotVMProcesses()

	// Create the virtual machine instance
	vm, err := c.createVirtualMachineInstance(ctx, cfg, disks, params)
	if err != nil {
		return
	}

	// Start the virtual machine
	startedAt := time.Now()
	err = vm.Start(ctx)
	if err != nil {
		c.eventRecorder.FailedToStartContainer(ctx, params.ContainerName, err)
		return
	}
	guard.Commit()
	go c.executions.Track(ctx, params.Namespace, params.Name, params.CPU, params.MemorySize, startedAt,
		execwindow.FindVMProcess(hostProcessesBefore), vm.Ended)
	c.eventRecorder.StartedContainer(ctx, params.ContainerName)

	if params.PostStartAction == nil {
		// Hookless pods gate Ready on the probe too (universal gating, see the mapper
		// in pkg/provider/virtualizationgroup_to_pod.go buildPodStatus). Assign the
		// function-level err (no block-scope shadow) so a permanent probe failure rides
		// the deferred finalizeVirtualMachineInfo to SetError -> State()=Failed -> PodFailed,
		// letting the controller recreate it - symmetric with the hook path below.
		if err = c.waitForVirtualMachineSSHReady(ctx, params.Namespace, params.Name); err != nil {
			// FailedPostStartProbe is the hookless analog of the hook path's
			// FailedPostStartHook. Suppress it on context.Canceled (pod deleted mid-probe is
			// normal teardown, not a failure; cf. the download path's guard above).
			if !errors.Is(err, context.Canceled) {
				c.eventRecorder.FailedPostStartProbe(ctx, params.ContainerName, err)
				log.G(ctx).WithError(err).Warn("hookless post-start SSH readiness probe failed; pod fails")
			}
			return
		}
		c.vms.Update(params.Namespace, params.Name, func(i vmdata.VirtualMachineInfo) vmdata.VirtualMachineInfo {
			i.Resource.SetPostStartFinishedAt(time.Now())
			return i
		})
		return
	}

	// Execute the post-start action
	err = c.execPostStartAction(ctx, params.Namespace, params.Name, *params.PostStartAction)
	if params.StopOnPostStart && !errors.Is(err, context.Canceled) {
		c.stopAfterPostStart(ctx, params, err)
	}
	if err != nil {
		// err propagates via the deferred finalizeVirtualMachineInfo to SetError -> State()=Failed
		// regardless of the guard below; only the cluster event is suppressed on a mid-hook pod
		// delete (ctx cancel), a normal teardown not a hook failure (cf. the context.Canceled
		// guard on the download path above).
		if !errors.Is(err, context.Canceled) {
			c.eventRecorder.FailedPostStartHook(ctx, params.ContainerName, params.PostStartAction.Command, err)
		}
		return
	}
	// Record success so the status mapper flips the macOS container Ready next poll.
	c.vms.Update(params.Namespace, params.Name, func(i vmdata.VirtualMachineInfo) vmdata.VirtualMachineInfo {
		i.Resource.SetPostStartFinishedAt(time.Now())
		return i
	})
}

// reportPull emits the pull outcome event for ref. A cancelled download is not reported, to
// avoid spamming the cluster events.
func (c *MacOSClient) reportPull(ctx context.Context, ref, containerName string, duration time.Duration, err error) {
	switch {
	case err == nil:
		c.eventRecorder.PulledImage(ctx, ref, containerName, duration.String())
	case !errors.Is(err, context.Canceled):
		c.eventRecorder.BackOffPullImage(ctx, ref, containerName, err)
	}
}

// dataDiskRefs lists the references of disks.
func dataDiskRefs(disks []DataDisk) (refs []string) {
	for _, d := range disks {
		refs = append(refs, d.Ref)
	}
	return refs
}

// stopAfterPostStart records the hook's exit status and stops the VM from the host, then
// frees its permit. The host-side stop is what makes Virtualization.framework release the VM
// slot; a guest power-off from the hook would leak it on macOS 26.5.
func (c *MacOSClient) stopAfterPostStart(ctx context.Context, params VirtualMachineParams, hookErr error) {
	logger := log.G(ctx)
	code, hasCode := hookExitStatus(hookErr)
	// The hook error is stored with the exit code and BEFORE the stop, so State() never shows
	// Terminated without an error for a failed hook.
	info, found := c.vms.Update(params.Namespace, params.Name, func(i vmdata.VirtualMachineInfo) vmdata.VirtualMachineInfo {
		if hasCode {
			i.Resource.SetHookExitCode(code)
		}
		if hookErr != nil {
			i.Resource.SetError(hookErr)
		}
		return i
	})
	if !found {
		// The pod was deleted meanwhile: DeleteVirtualMachine already stopped the VM and released
		// its permit. Releasing again would take another VM's permit.
		logger.Debug("virtual machine info expired; nothing to stop after the postStart hook")
		return
	}
	if instance := info.Resource.Instance(); instance != nil {
		// The pod context may be cancelled by now; the stop must still happen.
		if err := instance.Stop(context.WithoutCancel(ctx)); err != nil {
			logger.WithError(err).Warn("Failed to stop virtual machine from the host after the postStart hook exited")
		} else {
			logger.Infof("postStart hook exited (status %d, known=%t): virtual machine stopped from the host", code, hasCode)
		}
	}
	c.releasePermit(ctx, types.NamespacedName{Namespace: params.Namespace, Name: params.Name})
}

// hookExitStatus returns the exit status of a postStart hook result: 0 for success, the remote
// status for an exit error, and false for anything else (timeout, transport failure).
func hookExitStatus(err error) (int, bool) {
	if err == nil {
		return 0, true
	}
	if es, ok := errors.AsType[execerror.ExitStatusError](err); ok {
		return es.ExitStatus(), true
	}
	return 0, false
}

// finalizeVirtualMachineInfo updates the virtual machine info with the final result of the creation process.
func (c *MacOSClient) finalizeVirtualMachineInfo(ctx context.Context, params VirtualMachineParams, err error) {
	_, found := c.vms.Update(params.Namespace, params.Name, func(i vmdata.VirtualMachineInfo) vmdata.VirtualMachineInfo {
		i.DownloadCancelFunc = nil // indicate that download is no longer in progress
		if err != nil {
			i.Resource.SetError(err)
		}
		return i
	})
	if !found {
		log.G(ctx).Debug("virtual machine info expired")
	}
}

// createVirtualMachineInstance creates a new virtual machine instance with the specified parameters.
func (c *MacOSClient) createVirtualMachineInstance(ctx context.Context, cfg config.MacPlatformConfigurationOptions, disks []config.Disk, params VirtualMachineParams) (*vm.VirtualMachineInstance, error) {
	vm, err := setupVM(ctx, cfg, params.UID, params.CPU, params.MemorySize, c.networkInterfaceIdentifier, params.Mounts, disks)
	if err != nil {
		c.eventRecorder.FailedToCreateContainer(ctx, params.ContainerName, err)
		return nil, err
	}

	c.vms.Update(params.Namespace, params.Name, func(i vmdata.VirtualMachineInfo) vmdata.VirtualMachineInfo {
		i.Resource.SetInstance(vm)
		return i
	})
	c.eventRecorder.CreatedContainer(ctx, params.ContainerName)

	return vm, nil
}

// DeleteVirtualMachine stops and deletes the specified virtual machine.
func (c *MacOSClient) DeleteVirtualMachine(ctx context.Context, namespace string, name string, gracePeriod int64) (err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSClient.DeleteVirtualMachine")
	ctx = span.WithFields(ctx, log.Fields{
		"namespace":   namespace,
		"name":        name,
		"gracePeriod": gracePeriod,
	})
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	info, ok := c.vms.Load(namespace, name)
	if !ok {
		log.G(ctx).Debugf("virtual machine not found for namespace %s and name %s", namespace, name)
		return nil
	}

	// Registered first so it runs last: once the entry is deleted and the stack frame is dead,
	// let the Go finalizer release the VZVirtualMachine (Code-Hex/vz has no explicit release).
	defer runtime.GC()

	// Defers are LIFO: releasePermit runs first, then vms.Delete. releasePermit needs the entry
	// (it releases only a permit the entry records as acquired, and never drains for a missing
	// entry), so Delete must not run before it: reordering would leak every permit.
	defer c.vms.Delete(namespace, name)
	defer c.releasePermit(ctx, types.NamespacedName{Namespace: namespace, Name: name})

	if info.DownloadCancelFunc != nil {
		info.DownloadCancelFunc()
	}

	if instance := info.Resource.Instance(); instance != nil {
		err = c.stopVirtualMachine(ctx, instance, namespace, name, gracePeriod)
	}

	// Close the persistent SSH connection after stopVirtualMachine.
	if info.SSHConn != nil {
		_ = info.SSHConn.Close()
	}

	return err
}

// stopVirtualMachine stops the virtual machine instance.
func (c *MacOSClient) stopVirtualMachine(ctx context.Context, instance *vm.VirtualMachineInstance, namespace, name string, gracePeriod int64) (err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSClient.stopVirtualMachine")
	ctx = span.WithFields(ctx, log.Fields{
		"namespace":   namespace,
		"name":        name,
		"gracePeriod": gracePeriod,
	})
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	// Always stop from the host, never via a guest shutdown: on macOS 26.5 Virtualization.framework
	// only frees the active-VM slot for a host-side stop. gracePeriod is therefore unused.
	return instance.Stop(ctx)
}

// GetVirtualMachine retrieves the specified virtual machine.
func (c *MacOSClient) GetVirtualMachine(ctx context.Context, namespace string, name string) (i resource.MacOSVirtualMachine, err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSClient.GetVirtualMachine")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	info, err := c.getVirtualMachineInfo(ctx, namespace, name)
	if err != nil {
		return resource.MacOSVirtualMachine{}, err
	}

	return info.Resource, nil
}

// GetVirtualMachineListResult retrieves all virtual machines managed by the client.
func (c *MacOSClient) GetVirtualMachineListResult(ctx context.Context) (map[types.NamespacedName]resource.MacOSVirtualMachine, error) {
	_, span := trace.StartSpan(ctx, "MacOSClient.GetVirtualMachineListResult")
	defer span.End()

	vms := make(map[types.NamespacedName]resource.MacOSVirtualMachine)

	infos := c.vms.All()
	// simplify the map down to just the resource
	for key, info := range infos {
		vms[key] = info.Resource
	}

	return vms, nil
}

// getVirtualMachineInfo retrieves the virtual machine information.
func (c *MacOSClient) getVirtualMachineInfo(ctx context.Context, namespace, name string) (vmdata.VirtualMachineInfo, error) {
	info, ok := c.vms.Load(namespace, name)
	if !ok {
		log.G(ctx).Debugf("virtual machine not found for namespace %s and name %s", namespace, name)
		return vmdata.VirtualMachineInfo{}, errdefs.NotFound("virtual machine not found")
	}
	return info, nil
}

// setupVM creates a new virtual machine instance with the given parameters.
func setupVM(ctx context.Context, cfg config.MacPlatformConfigurationOptions, uid string, cpu uint, memorySize uint64, networkInterfaceIdentifier string, mounts []volumes.Mount, disks []config.Disk) (*vm.VirtualMachineInstance, error) {
	log.G(ctx).Debugf("Creating virtual machine with CPU: %d, memory: %d, network interface: %s, mounts: %+v", cpu, memorySize, networkInterfaceIdentifier, mounts)
	platformConfig, err := config.NewPlatformConfiguration(ctx, cfg, true, uid)
	if err != nil {
		return nil, fmt.Errorf("failed to create platform configuration: %w", err)
	}

	vmConfig, err := config.NewVirtualMachineConfiguration(ctx, platformConfig, cpu, memorySize, networkInterfaceIdentifier, mounts, disks)
	if err != nil {
		// Nothing owns the overlay clones yet, so remove them here.
		if platformConfig.IsOverlay {
			_ = os.Remove(platformConfig.BlockStoragePath)
			_ = os.Remove(platformConfig.AuxiliaryStoragePath)
		}
		return nil, fmt.Errorf("failed to create virtual machine configuration: %w", err)
	}

	vmInstance, err := vm.NewVirtualMachineInstance(ctx, vmConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create virtual machine instance: %w", err)
	}

	return vmInstance, nil
}
