package config

import (
	"context"
	"fmt"
	"net"
	"path/filepath"

	"github.com/agoda-com/macOS-vz-kubelet/internal/netutil"
	"github.com/agoda-com/macOS-vz-kubelet/internal/volumes"

	"github.com/Code-Hex/vz/v3"
	"github.com/virtual-kubelet/virtual-kubelet/trace"
)

// Location for all the shared directories inside macOS
const MacOSSharedDirectoryPath = "/Volumes/My Shared Files"

// VirtualMachineConfiguration encapsulates configuration details for a virtual machine, including network and storage.
type VirtualMachineConfiguration struct {
	MACAddress       net.HardwareAddr
	NetworkInterface string

	overlayBlockStoragePath     string
	overlayAuxiliaryStoragePath string

	*vz.VirtualMachineConfiguration
}

// Disk is an extra virtio block device attached after the boot disk.
type Disk struct {
	Path     string
	ReadOnly bool
}

// NewVirtualMachineConfiguration initializes a new virtual machine configuration with provided settings.
func NewVirtualMachineConfiguration(ctx context.Context, platformConfig *PlatformConfiguration, cpuCount uint, memorySize uint64, networkInterfaceIdentifier string, mounts []volumes.Mount, disks []Disk) (p *VirtualMachineConfiguration, err error) {
	ctx, span := trace.StartSpan(ctx, "vm.NewVirtualMachineConfiguration")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	// Create a new macOS bootloader
	bootloader, err := vz.NewMacOSBootLoader()
	if err != nil {
		return nil, fmt.Errorf("failed to create a new macOS bootloader: %w", err)
	}

	// Create a new virtual machine configuration
	config, err := vz.NewVirtualMachineConfiguration(bootloader, cpuCount, memorySize)
	if err != nil {
		return nil, fmt.Errorf("failed to create a new virtual machine configuration: %w", err)
	}

	// Generate MAC address
	macAddrStr, err := netutil.GenerateRandMAC()
	if err != nil {
		return nil, fmt.Errorf("failed to generate random mac address: %w", err)
	}
	macAddr, err := net.ParseMAC(macAddrStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse mac address: %w", err)
	}

	// Attach device configurations
	if err = attachDeviceConfigurations(ctx, config, platformConfig, networkInterfaceIdentifier, macAddr, disks); err != nil {
		return nil, fmt.Errorf("failed to attach device configurations: %w", err)
	}

	// Attach volumes
	if err = attachDirectorySharingDevicesConfiguration(ctx, config, mounts); err != nil {
		return nil, fmt.Errorf("failed to attach volume mounts configurations: %w", err)
	}

	// Validate the configuration
	validated, err := config.Validate()
	if err != nil {
		return nil, fmt.Errorf("failed to validate configuration: %w", err)
	}
	if !validated {
		return nil, fmt.Errorf("invalid configuration")
	}

	p = &VirtualMachineConfiguration{
		MACAddress:       macAddr,
		NetworkInterface: networkInterfaceIdentifier,

		VirtualMachineConfiguration: config,
	}

	if platformConfig.IsOverlay {
		p.overlayBlockStoragePath = platformConfig.BlockStoragePath
		p.overlayAuxiliaryStoragePath = platformConfig.AuxiliaryStoragePath
	}

	return p, nil
}

// GetOverlays returns the overlay paths if they are in use; otherwise, returns an empty string.
func (c *VirtualMachineConfiguration) GetOverlays() (overlayBlockStoragePath string, overlayAuxiliaryStoragePath string, ok bool) {
	return c.overlayBlockStoragePath, c.overlayAuxiliaryStoragePath, c.overlayBlockStoragePath != "" && c.overlayAuxiliaryStoragePath != ""
}

// attachDeviceConfigurations encapsulates various device and configuration attachments to the VM.
func attachDeviceConfigurations(ctx context.Context, config *vz.VirtualMachineConfiguration, platformConfig *PlatformConfiguration, networkInterfaceIdentifier string, mac net.HardwareAddr, disks []Disk) (err error) {
	_, span := trace.StartSpan(ctx, "vm.attachDeviceConfigurations")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	// Set the platform configuration
	config.SetPlatformVirtualMachineConfiguration(platformConfig)

	// Create a graphics device configuration
	graphicsDeviceConfig, err := createGraphicsDeviceConfiguration()
	if err != nil {
		return fmt.Errorf("failed to create graphics device configuration: %w", err)
	}
	config.SetGraphicsDevicesVirtualMachineConfiguration([]vz.GraphicsDeviceConfiguration{
		graphicsDeviceConfig,
	})

	// Attach the disk image to the virtual machine
	diskImageAttachment, err := vz.NewDiskImageStorageDeviceAttachment(
		platformConfig.BlockStoragePath,
		false,
	)
	if err != nil {
		return fmt.Errorf("failed to create disk image storage device attachment: %w", err)
	}
	blockDeviceConfig, err := vz.NewVirtioBlockDeviceConfiguration(diskImageAttachment)
	if err != nil {
		return fmt.Errorf("failed to create block device configuration: %w", err)
	}
	storageDevices := []vz.StorageDeviceConfiguration{blockDeviceConfig}
	for _, d := range disks {
		attachment, err := vz.NewDiskImageStorageDeviceAttachment(d.Path, d.ReadOnly)
		if err != nil {
			return fmt.Errorf("failed to create disk image storage device attachment for %s: %w", d.Path, err)
		}
		dev, err := vz.NewVirtioBlockDeviceConfiguration(attachment)
		if err != nil {
			return fmt.Errorf("failed to create block device configuration for %s: %w", d.Path, err)
		}
		storageDevices = append(storageDevices, dev)
	}
	config.SetStorageDevicesVirtualMachineConfiguration(storageDevices)

	// Create a network device configuration
	networkDeviceConfig, err := createNetworkDeviceConfiguration(networkInterfaceIdentifier)
	if err != nil {
		return fmt.Errorf("failed to create network device configuration: %w", err)
	}
	// Set the MAC address
	macAddr, err := vz.NewMACAddress(mac)
	if err != nil {
		return fmt.Errorf("failed to create mac address: %w", err)
	}
	networkDeviceConfig.SetMACAddress(macAddr)
	config.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{
		networkDeviceConfig,
	})

	// Create a pointing device configuration
	usbScreenPointingDevice, err := vz.NewUSBScreenCoordinatePointingDeviceConfiguration()
	if err != nil {
		return fmt.Errorf("failed to create pointing device configuration: %w", err)
	}
	pointingDevices := []vz.PointingDeviceConfiguration{usbScreenPointingDevice}
	trackpad, err := vz.NewMacTrackpadConfiguration()
	if err == nil {
		pointingDevices = append(pointingDevices, trackpad)
	}
	config.SetPointingDevicesVirtualMachineConfiguration(pointingDevices)

	// Create keyboard device configuration
	keyboardDeviceConfig, err := vz.NewUSBKeyboardConfiguration()
	if err != nil {
		return fmt.Errorf("failed to create keyboard device configuration: %w", err)
	}
	config.SetKeyboardsVirtualMachineConfiguration([]vz.KeyboardConfiguration{
		keyboardDeviceConfig,
	})

	// Create audio device configuration
	audioDeviceConfig, err := createAudioDeviceConfiguration()
	if err != nil {
		return fmt.Errorf("failed to create audio device configuration: %w", err)
	}
	config.SetAudioDevicesVirtualMachineConfiguration([]vz.AudioDeviceConfiguration{
		audioDeviceConfig,
	})

	return attachTartCompanionDevices(config)
}

// attachTartCompanionDevices adds the devices Tart gives every VM and this provider did not: virtio
// entropy, a virtio console with one named port and a vsock device. Images built for Tart (the
// Cirrus Labs bases) are booted and tested with them. The Cirrus Sonoma base (14.8.7) booted on
// a macOS 26.6 host without them restarted itself every ~15 s ("System shutdown initiated by
// kernel"), while the same disk, NVRAM, hardware model and machine identifier stayed up under
// Tart, as root and as a user (2026-10-22).
func attachTartCompanionDevices(config *vz.VirtualMachineConfiguration) error {
	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return fmt.Errorf("failed to create entropy device configuration: %w", err)
	}
	config.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})

	console, err := vz.NewVirtioConsoleDeviceConfiguration()
	if err != nil {
		return fmt.Errorf("failed to create console device configuration: %w", err)
	}
	port, err := vz.NewVirtioConsolePortConfiguration(vz.WithVirtioConsolePortConfigurationName("macos-vz-kubelet"))
	if err != nil {
		return fmt.Errorf("failed to create console port configuration: %w", err)
	}
	console.SetVirtioConsolePortConfiguration(0, port)
	config.SetConsoleDevicesVirtualMachineConfiguration([]vz.ConsoleDeviceConfiguration{console})

	socket, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return fmt.Errorf("failed to create socket device configuration: %w", err)
	}
	config.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{socket})
	return nil
}

// createGraphicsDeviceConfiguration creates a new graphics device configuration.
// While we run VM headless, we still need to create a graphics device configuration to support VNC.
func createGraphicsDeviceConfiguration() (*vz.MacGraphicsDeviceConfiguration, error) {
	graphicDeviceConfig, err := vz.NewMacGraphicsDeviceConfiguration()
	if err != nil {
		return nil, err
	}
	graphicsDisplayConfig, err := vz.NewMacGraphicsDisplayConfiguration(1920, 1200, 72) // Tart's pixel density
	if err != nil {
		return nil, err
	}
	graphicDeviceConfig.SetDisplays(
		graphicsDisplayConfig,
	)
	return graphicDeviceConfig, nil
}

// createNetworkDeviceConfiguration creates a network attachment using provided identifier if not empty; otherwise, uses NAT.
func createNetworkDeviceConfiguration(networkInterfaceIdentifier string) (*vz.VirtioNetworkDeviceConfiguration, error) {
	var attachment vz.NetworkDeviceAttachment
	var err error
	if networkInterfaceIdentifier != "" {
		var networkInterface vz.BridgedNetwork
		networkInterfaces := vz.NetworkInterfaces()
		for _, b := range networkInterfaces {
			if b.Identifier() == networkInterfaceIdentifier {
				networkInterface = b
				break
			}
		}
		if networkInterface == nil {
			return nil, fmt.Errorf("network interface %s not found", networkInterfaceIdentifier)
		}

		attachment, err = vz.NewBridgedNetworkDeviceAttachment(networkInterface)
		if err != nil {
			return nil, err
		}
	} else {
		attachment, err = vz.NewNATNetworkDeviceAttachment()
		if err != nil {
			return nil, err
		}
	}

	return vz.NewVirtioNetworkDeviceConfiguration(attachment)
}

// createAudioDeviceConfiguration creates a new audio device configuration.
func createAudioDeviceConfiguration() (*vz.VirtioSoundDeviceConfiguration, error) {
	audioConfig, err := vz.NewVirtioSoundDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("failed to create sound device configuration: %w", err)
	}
	inputStream, err := vz.NewVirtioSoundDeviceHostInputStreamConfiguration()
	if err != nil {
		return nil, fmt.Errorf("failed to create input stream configuration: %w", err)
	}
	outputStream, err := vz.NewVirtioSoundDeviceHostOutputStreamConfiguration()
	if err != nil {
		return nil, fmt.Errorf("failed to create output stream configuration: %w", err)
	}
	audioConfig.SetStreams(
		inputStream,
		outputStream,
	)
	return audioConfig, nil
}

// attachDirectorySharingDevicesConfiguration attaches directory sharing devices to the virtual machine configuration.
// The function creates shared directories based on the provided mounts and configures the virtual machine to use these shared directories.
func attachDirectorySharingDevicesConfiguration(ctx context.Context, config *vz.VirtualMachineConfiguration, mounts []volumes.Mount) (err error) {
	_, span := trace.StartSpan(ctx, "vm.attachDirectorySharingDevicesConfiguration")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	if len(mounts) == 0 {
		return nil
	}

	automountTag, err := vz.MacOSGuestAutomountTag()
	if err != nil {
		return fmt.Errorf("failed to get macOS guest automount tag: %w", err)
	}

	sharedDirs := make(map[string]*vz.SharedDirectory, len(mounts))
	for _, v := range mounts {
		if err := v.Revalidate(); err != nil {
			return err
		}
		sharedDir, err := vz.NewSharedDirectory(v.HostPath, v.ReadOnly)
		if err != nil {
			return fmt.Errorf("failed to create shared directory: %w", err)
		}
		sharedDirs[filepath.Base(v.ContainerPath)] = sharedDir
	}

	directoryShare, err := vz.NewMultipleDirectoryShare(sharedDirs)
	if err != nil {
		return fmt.Errorf("failed to create directory share: %w", err)
	}

	fsConfig, err := vz.NewVirtioFileSystemDeviceConfiguration(automountTag)
	if err != nil {
		return fmt.Errorf("failed to create file system device configuration: %w", err)
	}
	fsConfig.SetDirectoryShare(directoryShare)

	config.SetDirectorySharingDevicesVirtualMachineConfiguration([]vz.DirectorySharingDeviceConfiguration{
		fsConfig,
	})
	return nil
}
