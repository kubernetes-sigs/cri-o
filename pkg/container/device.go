package container

import (
	"os"
	"path/filepath"
	"strings"

	devicecfg "github.com/cri-o/cri-o/internal/config/device"
	crioann "github.com/cri-o/cri-o/pkg/annotations"
	"github.com/cri-o/cri-o/server/cri/types"
	"github.com/cri-o/cri-o/utils"
	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/opencontainers/runc/libcontainer/devices"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
)

func (c *container) SpecAddDevices(configuredDevices, annotationDevices []devicecfg.Device, privilegedWithoutHostDevices bool) error {
	// First, clear the existing devices from the spec
	c.Spec().Config.Linux.Devices = []rspec.LinuxDevice{}

	// After that, add additional_devices from config
	for i := range configuredDevices {
		d := &configuredDevices[i]

		c.Spec().AddDevice(d.Device)
		c.Spec().AddLinuxResourcesDevice(d.Resource.Allow, d.Resource.Type, d.Resource.Major, d.Resource.Minor, d.Resource.Access)
	}

	// Next, verify and add the devices from annotations
	for i := range annotationDevices {
		d := &annotationDevices[i]

		c.Spec().AddDevice(d.Device)
		c.Spec().AddLinuxResourcesDevice(d.Resource.Allow, d.Resource.Type, d.Resource.Major, d.Resource.Minor, d.Resource.Access)
	}

	// Then, add host devices if privileged
	if err := c.specAddHostDevicesIfPrivileged(privilegedWithoutHostDevices); err != nil {
		return err
	}

	// Finally, add container config devices
	return c.specAddContainerConfigDevices()
}

func (c *container) specAddHostDevicesIfPrivileged(privilegedWithoutHostDevices bool) error {
	if !c.Privileged() || privilegedWithoutHostDevices {
		return nil
	}
	hostDevices, err := devices.HostDevices()
	if err != nil {
		return err
	}
	for _, hostDevice := range hostDevices {
		rd := rspec.LinuxDevice{
			Path:  hostDevice.Path,
			Type:  string(hostDevice.Type),
			Major: hostDevice.Major,
			Minor: hostDevice.Minor,
			UID:   &hostDevice.Uid,
			GID:   &hostDevice.Gid,
		}
		if hostDevice.Major == 0 && hostDevice.Minor == 0 {
			// Invalid device, most likely a symbolic link, skip it.
			continue
		}
		c.Spec().AddDevice(rd)
	}
	c.Spec().Config.Linux.Resources.Devices = []rspec.LinuxDeviceCgroup{
		{
			Allow:  true,
			Access: "rwm",
		},
	}
	return nil
}

func (c *container) specAddContainerConfigDevices() error {
	sp := c.Spec().Config

	disableDeviceOwnershipFromSecurityContext := checkDisableDeviceOwnershipFromSecurityContextAnnotation(c.Config().Annotations)

	for _, device := range c.Config().Devices {
		// pin the device to avoid using `device` within the range scope as
		// wrong function literal
		device := device

		// If we are privileged, we have access to devices on the host.
		// If the requested container path already exists on the host, the container won't see the expected host path.
		// Therefore, we must error out if the container path already exists
		if c.Privileged() && device.ContainerPath != device.HostPath {
			// we expect this to not exist
			_, err := os.Stat(device.ContainerPath)
			if err == nil {
				return errors.Errorf("privileged container was configured with a device container path that already exists on the host.")
			}
			if !os.IsNotExist(err) {
				return errors.Wrap(err, "error checking if container path exists on host")
			}
		}

		path, err := securejoin.SecureJoin("/", device.HostPath)
		if err != nil {
			return err
		}
		dev, err := devices.DeviceFromPath(path, device.Permissions)
		// if there was no error, return the device
		if err == nil {
			rd := rspec.LinuxDevice{
				Path:  device.ContainerPath,
				Type:  string(dev.Type),
				Major: dev.Major,
				Minor: dev.Minor,
				UID:   getDeviceUID(c.Config(), dev.Uid, disableDeviceOwnershipFromSecurityContext),
				GID:   getDeviceGID(c.Config(), dev.Gid, disableDeviceOwnershipFromSecurityContext),
			}
			c.Spec().AddDevice(rd)
			sp.Linux.Resources.Devices = append(sp.Linux.Resources.Devices, rspec.LinuxDeviceCgroup{
				Allow:  true,
				Type:   string(dev.Type),
				Major:  &dev.Major,
				Minor:  &dev.Minor,
				Access: string(dev.Permissions),
			})
			continue
		}
		// if the device is not a device node
		// try to see if it's a directory holding many devices
		if err == devices.ErrNotADevice {
			// check if it is a directory
			if e := utils.IsDirectory(path); e == nil {
				// mount the internal devices recursively
				// nolint: errcheck
				filepath.Walk(path, func(dpath string, f os.FileInfo, e error) error {
					// filepath.Walk failed, skip
					if e != nil {
						return nil
					}
					childDevice, e := devices.DeviceFromPath(dpath, device.Permissions)
					if e != nil {
						// ignore the device
						return nil
					}
					cPath := strings.Replace(dpath, path, device.ContainerPath, 1)
					rd := rspec.LinuxDevice{
						Path:  cPath,
						Type:  string(childDevice.Type),
						Major: childDevice.Major,
						Minor: childDevice.Minor,
						UID:   &childDevice.Uid,
						GID:   &childDevice.Gid,
					}
					c.Spec().AddDevice(rd)
					sp.Linux.Resources.Devices = append(sp.Linux.Resources.Devices, rspec.LinuxDeviceCgroup{
						Allow:  true,
						Type:   string(childDevice.Type),
						Major:  &childDevice.Major,
						Minor:  &childDevice.Minor,
						Access: string(childDevice.Permissions),
					})

					return nil
				})
			}
		}
	}
	return nil
}

func checkDisableDeviceOwnershipFromSecurityContextAnnotation(annotations map[string]string) bool {
	if v, ok := annotations[crioann.DisableDeviceOwnershipFromSecurityContextAnnotation]; ok {
		if v == "true" {
			return true
		}
	}
	return false
}

// getDeviceUID() and getDeviceGID() are used to find the right
// uid/gid values for the device node created in the container
// namespace. The runtime executes mknod() and chmod()s the created
// device with the values returned here.
//
// TODO(mythi): In case of user namespaces, the runtime simply bind
// mounts the the devices from the host. Additional logic is needed
// to check that the runtimes effective UID/GID on the host has the
// permissions to access the device node and/or the right user namespace
// mappings are created.
//
// CRI-O has an experimental support for setting user namespace mappings
// via annotations when pod's securitycontext runs as root/uid=0. When
// enabled, the logic below does not change the behavior for existing
// mappings unless container's securitycontext user/group overrides what
// is set for the pod.
//
// Ref: https://github.com/kubernetes/kubernetes/issues/92211
func getDeviceUID(config *types.ContainerConfig, hostUID uint32, disableDeviceOwnershipFromSecurityContext bool) *uint32 {
	if userval := config.Linux.SecurityContext.RunAsUser; userval != nil {
		uid := uint32(userval.Value)
		if uid > 0 && !disableDeviceOwnershipFromSecurityContext {
			return &uid
		}
	}
	return &hostUID
}

func getDeviceGID(config *types.ContainerConfig, hostGID uint32, disableDeviceOwnershipFromSecurityContext bool) *uint32 {
	if groupval := config.Linux.SecurityContext.RunAsGroup; groupval != nil {
		gid := uint32(groupval.Value)
		if gid > 0 && !disableDeviceOwnershipFromSecurityContext {
			return &gid
		}
	}
	return &hostGID
}
