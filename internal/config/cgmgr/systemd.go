// +build linux

package cgmgr

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/containers/podman/v3/pkg/rootless"
	systemdDbus "github.com/coreos/go-systemd/v22/dbus"
	"github.com/cri-o/cri-o/internal/config/node"
	"github.com/cri-o/cri-o/internal/dbusmgr"
	"github.com/cri-o/cri-o/utils"
	"github.com/godbus/dbus/v5"
	"github.com/opencontainers/runc/libcontainer/cgroups/systemd"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

const defaultSystemdParent = "system.slice"

// SystemdManager is the parent type of SystemdV{1,2}Manager.
// it defines all of the common functionality between V1 and V2
type SystemdManager struct {
	memoryPath, memoryMaxFile string
	dbusMgr                   *dbusmgr.DbusConnManager
}

func NewSystemdManager() *SystemdManager {
	systemdMgr := SystemdManager{
		memoryPath:    cgroupMemoryPathV1,
		memoryMaxFile: cgroupMemoryMaxFileV1,
	}
	if node.CgroupIsV2() {
		systemdMgr.memoryPath = cgroupMemoryPathV2
		systemdMgr.memoryMaxFile = cgroupMemoryMaxFileV2
	}
	systemdMgr.dbusMgr = dbusmgr.NewDbusConnManager(rootless.IsRootless())

	return &systemdMgr
}

// Name returns the name of the cgroup manager (systemd)
func (*SystemdManager) Name() string {
	return systemdCgroupManager
}

// IsSystemd returns that it is a systemd cgroup manager
func (*SystemdManager) IsSystemd() bool {
	return true
}

// ContainerCgroupPath takes arguments sandbox parent cgroup and container ID and returns
// the cgroup path for that containerID. If parentCgroup is empty, it
// uses the default parent system.slice
func (*SystemdManager) ContainerCgroupPath(sbParent, containerID string) string {
	parent := defaultSystemdParent
	if sbParent != "" {
		parent = sbParent
	}
	return parent + ":" + crioPrefix + ":" + containerID
}

// ContainerCgroupAbsolutePath takes arguments sandbox parent cgroup and container ID and
// returns the cgroup path on disk for that containerID. If parentCgroup is empty, it
// uses the default parent system.slice
func (*SystemdManager) ContainerCgroupAbsolutePath(sbParent, containerID string) (string, error) {
	parent := defaultSystemdParent
	if sbParent != "" {
		parent = sbParent
	}
	logrus.Debugf("Expanding systemd cgroup slice %v", parent)
	cgroup, err := systemd.ExpandSlice(parent)
	if err != nil {
		return "", errors.Wrapf(err, "error expanding systemd slice to get container %s stats", containerID)
	}

	return filepath.Join(cgroup, crioPrefix+"-"+containerID+".scope"), nil
}

// MoveConmonToCgroup takes the container ID, cgroup parent, conmon's cgroup (from the config) and conmon's PID
// It attempts to move conmon to the correct cgroup.
// cgroupPathToClean should always be returned empty. It is part of the interface to return the cgroup path
// that cri-o is responsible for cleaning up upon the container's death.
// Systemd takes care of this cleaning for us, so return an empty string
func (m *SystemdManager) MoveConmonToCgroup(cid, cgroupParent, conmonCgroup string, pid int, resources *rspec.LinuxResources) (cgroupPathToClean string, _ error) {
	if strings.HasSuffix(conmonCgroup, ".slice") {
		cgroupParent = conmonCgroup
	}
	conmonUnitName := fmt.Sprintf("crio-conmon-%s.scope", cid)

	// Set the systemd KillSignal to SIGPIPE that conmon ignores.
	// This helps during node shutdown so that conmon waits for the container
	// to exit and doesn't forward the SIGTERM that it gets.
	props := []systemdDbus.Property{
		{
			Name:  "KillSignal",
			Value: dbus.MakeVariant(int(unix.SIGPIPE)),
		},
		systemdDbus.PropAfter("crio.service"),
	}

	if resources != nil && resources.CPU != nil {
		if resources.CPU.Cpus != "" {
			if !node.SystemdHasAllowedCPUs() {
				logrus.Errorf("Systemd does not support AllowedCPUs; skipping setting for workload")
			} else {
				bits, err := systemd.RangeToBits(resources.CPU.Cpus)
				if err != nil {
					return "", errors.Wrapf(err, "cpuset conversion error")
				}
				props = append(props, systemdDbus.Property{
					Name:  "AllowedCPUs",
					Value: dbus.MakeVariant(bits),
				})
			}
		}
		if resources.CPU.Shares != nil {
			props = append(props, systemdDbus.Property{
				Name:  "CPUShares",
				Value: dbus.MakeVariant(resources.CPU.Shares),
			})
		}
	}

	logrus.Debugf("Running conmon under slice %s and unitName %s", cgroupParent, conmonUnitName)
	if err := utils.RunUnderSystemdScope(m.dbusMgr, pid, cgroupParent, conmonUnitName, props...); err != nil {
		return "", errors.Wrapf(err, "failed to add conmon to systemd sandbox cgroup")
	}
	// return empty string as path because cgroup cleanup is done by systemd
	return "", nil
}

// SandboxCgroupPath takes the sandbox parent, and sandbox ID. It
// returns the cgroup parent, cgroup path, and error.
// It also checks there is enough memory in the given cgroup
func (m *SystemdManager) SandboxCgroupPath(sbParent, sbID string) (cgParent, cgPath string, _ error) {
	if sbParent == "" {
		return "", "", nil
	}

	if !strings.HasSuffix(filepath.Base(sbParent), ".slice") {
		return "", "", fmt.Errorf("cri-o configured with systemd cgroup manager, but did not receive slice as parent: %s", sbParent)
	}

	cgParent = convertCgroupFsNameToSystemd(sbParent)
	slicePath, err := systemd.ExpandSlice(cgParent)
	if err != nil {
		return "", "", errors.Wrapf(err, "expanding systemd slice path for %q", cgParent)
	}

	if err := verifyCgroupHasEnoughMemory(slicePath, m.memoryPath, m.memoryMaxFile); err != nil {
		return "", "", err
	}

	cgPath = cgParent + ":" + crioPrefix + ":" + sbID

	return cgParent, cgPath, nil
}

// convertCgroupFsNameToSystemd converts an expanded cgroupfs name to its systemd name.
// For example, it will convert test.slice/test-a.slice/test-a-b.slice to become test-a-b.slice
func convertCgroupFsNameToSystemd(cgroupfsName string) string {
	// TODO: see if libcontainer systemd implementation could use something similar, and if so, move
	// this function up to that library.  At that time, it would most likely do validation specific to systemd
	// above and beyond the simple assumption here that the base of the path encodes the hierarchy
	// per systemd convention.
	return path.Base(cgroupfsName)
}

// CreateSandboxCgroup calls the helper function createSandboxCgroup for this manager.
func (m *SystemdManager) CreateSandboxCgroup(sbParent, containerID string) error {
	return createSandboxCgroup(sbParent, containerID, m)
}

// RemoveSandboxCgroup calls the helper function removeSandboxCgroup for this manager.
func (m *SystemdManager) RemoveSandboxCgroup(sbParent, containerID string) error {
	return removeSandboxCgroup(sbParent, containerID, m)
}
