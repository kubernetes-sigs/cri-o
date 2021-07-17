// +build linux

package cgmgr

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/containers/podman/v3/pkg/cgroups"
	"github.com/cri-o/cri-o/internal/config/node"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	crioPrefix = "crio"
	// minMemoryLimit is the minimum memory that must be set for a container.
	// A lower value would result in the container failing to start.
	// this value has been arrived at for runc on x86_64 hardware
	minMemoryLimit = 12 * 1024 * 1024
	// CgroupfsCgroupManager represents cgroupfs native cgroup manager
	cgroupfsCgroupManager = "cgroupfs"
	// SystemdCgroupManager represents systemd native cgroup manager
	systemdCgroupManager = "systemd"

	DefaultCgroupManager = systemdCgroupManager

	// these constants define the path and name of the memory max file
	// for v1 and v2 respectively
	cgroupMemoryPathV1    = "/sys/fs/cgroup/memory"
	cgroupMemoryMaxFileV1 = "memory.limit_in_bytes"
	cgroupMemoryPathV2    = "/sys/fs/cgroup"
	cgroupMemoryMaxFileV2 = "memory.max"
)

// CgroupManager is an interface to interact with cgroups on a node. CRI-O is configured at startup to either use
// systemd or cgroupfs, and the node itself is booted with cgroup v1, or cgroup v2. CgroupManager is an interface for
// the CRI-O server to use cgroups, regardless of how it or the node was configured.
type CgroupManager interface {
	// String returns the name of the cgroup manager (either cgroupfs or systemd)
	Name() string
	// IsSystemd returns whether it is a systemd cgroup manager
	IsSystemd() bool
	// ContainerCgroupPath takes arguments sandbox parent cgroup and container ID and returns
	// the cgroup path for that containerID. If parentCgroup is empty, it
	// uses the default parent for that particular manager
	ContainerCgroupPath(string, string) string
	// ContainerCgroupAbsolutePath takes arguments sandbox parent cgroup and container ID and
	// returns the cgroup path on disk for that containerID. If parentCgroup is empty, it
	// uses the default parent for that particular manager
	ContainerCgroupAbsolutePath(string, string) (string, error)
	// SandboxCgroupPath takes the sandbox parent, and sandbox ID. It
	// returns the cgroup parent, cgroup path, and error. For systemd cgroups,
	// it also checks there is enough memory in the given cgroup
	SandboxCgroupPath(string, string) (string, string, error)
	// MoveConmonToCgroup takes the container ID, cgroup parent, conmon's cgroup (from the config), conmon's PID, and some customized resources
	// It attempts to move conmon to the correct cgroup, and set the resources for that cgroup.
	// It returns the cgroupfs parent that conmon was put into
	// so that CRI-O can clean the parent cgroup of the newly added conmon once the process terminates (systemd handles this for us)
	MoveConmonToCgroup(cid, cgroupParent, conmonCgroup string, pid int, resources *rspec.LinuxResources) (string, error)
	// CreateSandboxCgroup takes the sandbox parent, and sandbox ID.
	// It creates a new cgroup for that sandbox, which is useful when spoofing an infra container.
	CreateSandboxCgroup(sbParent, containerID string) error
	// RemoveSandboxCgroup takes the sandbox parent, and sandbox ID.
	// It loads a cgroup for that sandbox, and then deletes it.
	RemoveSandboxCgroup(sbParent, containerID string) error
}

// New creates a new CgroupManager with defaults
func New() CgroupManager {
	cm, err := SetCgroupManager(DefaultCgroupManager)
	if err != nil {
		panic(err)
	}
	return cm
}

// SetCgroupManager takes a string and branches on it to return
// the type of cgroup manager configured
func SetCgroupManager(cgroupManager string) (CgroupManager, error) {
	switch cgroupManager {
	case systemdCgroupManager:
		return NewSystemdManager(), nil
	case cgroupfsCgroupManager:
		cgroupfsMgr := CgroupfsManager{
			memoryPath:    cgroupMemoryPathV1,
			memoryMaxFile: cgroupMemoryMaxFileV1,
		}
		if node.CgroupIsV2() {
			cgroupfsMgr.memoryPath = cgroupMemoryPathV2
			cgroupfsMgr.memoryMaxFile = cgroupMemoryMaxFileV2
		}
		return &cgroupfsMgr, nil
	default:
		return nil, errors.Errorf("invalid cgroup manager: %s", cgroupManager)
	}
}

func verifyCgroupHasEnoughMemory(slicePath, memorySubsystemPath, memoryMaxFilename string) error {
	// read in the memory limit from memory max file
	fileData, err := ioutil.ReadFile(filepath.Join(memorySubsystemPath, slicePath, memoryMaxFilename))
	if err != nil {
		if os.IsNotExist(err) {
			logrus.Warnf("Failed to find %s at path: %q", memoryMaxFilename, slicePath)
			return nil
		}
		return errors.Wrapf(err, "unable to read memory file for cgroups at %s", slicePath)
	}

	// strip off the newline character and convert it to an int
	strMemory := strings.TrimRight(string(fileData), "\n")
	if strMemory != "" && strMemory != "max" {
		memoryLimit, err := strconv.ParseInt(strMemory, 10, 64)
		if err != nil {
			return errors.Wrapf(err, "error converting cgroup memory value from string to int %q", strMemory)
		}
		// Compare with the minimum allowed memory limit
		if err := VerifyMemoryIsEnough(memoryLimit); err != nil {
			return errors.Errorf("pod %v", err)
		}
	}
	return nil
}

// VerifyMemoryIsEnough verifies that the cgroup memory limit is above a specified minimum memory limit.
func VerifyMemoryIsEnough(memoryLimit int64) error {
	if memoryLimit != 0 && memoryLimit < minMemoryLimit {
		return fmt.Errorf("set memory limit %d too low; should be at least %d", memoryLimit, minMemoryLimit)
	}
	return nil
}

// createSandboxCgroup takes the sandbox parent, and sandbox ID.
// It creates a new cgroup for that sandbox, which is useful when spoofing an infra container.
func createSandboxCgroup(sbParent, containerID string, mgr CgroupManager) error {
	path, err := mgr.ContainerCgroupAbsolutePath(sbParent, containerID)
	if err != nil {
		return err
	}
	_, err = cgroups.New(path, &rspec.LinuxResources{})
	return err
}

// removeSandboxCgroup takes the sandbox parent, and sandbox ID.
// It loads a cgroup for that sandbox, and then deletes it.
func removeSandboxCgroup(sbParent, containerID string, mgr CgroupManager) error {
	path, err := mgr.ContainerCgroupAbsolutePath(sbParent, containerID)
	if err != nil {
		return err
	}
	cg, err := cgroups.Load(path)
	if err != nil {
		return err
	}
	return cg.Delete()
}
