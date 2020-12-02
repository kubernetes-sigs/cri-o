// +build linux darwin

package utils

import (
	"bufio"
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/containers/podman/v2/pkg/cgroups"
	"github.com/containers/podman/v2/pkg/rootless"
	systemdDbus "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// RunUnderSystemdScope adds the specified pid to a systemd scope
func RunUnderSystemdScope(pid int, slice string, unitName string) error {
	var properties []systemdDbus.Property
	var conn *systemdDbus.Conn
	var err error

	if rootless.IsRootless() {
		conn, err = cgroups.GetUserConnection(rootless.GetRootlessUID())
		if err != nil {
			return err
		}
	} else {
		conn, err = systemdDbus.New()
		if err != nil {
			return err
		}
	}
	properties = append(properties, systemdDbus.PropSlice(slice))
	properties = append(properties, newProp("PIDs", []uint32{uint32(pid)}))
	properties = append(properties, newProp("Delegate", true))
	properties = append(properties, newProp("DefaultDependencies", false))
	ch := make(chan string)
	_, err = conn.StartTransientUnit(unitName, "replace", properties, ch)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Block until job is started
	<-ch

	return nil
}

func getCgroupProcess(procFile string) (string, error) {
	f, err := os.Open(procFile)
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	cgroup := "/"
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			return "", errors.Errorf("cannot parse cgroup line %q", line)
		}
		if strings.HasPrefix(line, "0::") {
			cgroup = line[3:]
			break
		}
		// root cgroup, skip it
		if parts[2] == "/" {
			continue
		}
		// The process must have the same cgroup path for all controllers
		// The OCI runtime spec file allow us to specify only one path.
		if cgroup != "/" && cgroup != parts[2] {
			return "", errors.Errorf("cgroup configuration not supported, the process is in two different cgroups")
		}
		cgroup = parts[2]
	}
	if cgroup == "/" {
		return "", errors.Errorf("could not find cgroup mount in %q", procFile)
	}
	return cgroup, nil
}

// GetOwnCgroup returns the cgroup for the current process.
func GetOwnCgroup() (string, error) {
	return getCgroupProcess("/proc/self/cgroup")
}

// GetCgroupProcess returns the cgroup for the specified process process.
func GetCgroupProcess(pid int) (string, error) {
	return getCgroupProcess(fmt.Sprintf("/proc/%d/cgroup", pid))
}

// MoveUnderCgroupSubtree moves the PID under a cgroup subtree.
func MoveUnderCgroupSubtree(subtree string) error {
	procFile := "/proc/self/cgroup"
	f, err := os.Open(procFile)
	if err != nil {
		return err
	}
	defer f.Close()

	unifiedMode, err := cgroups.IsCgroup2UnifiedMode()
	if err != nil {
		return err
	}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			return errors.Errorf("cannot parse cgroup line %q", line)
		}

		// root cgroup, skip it
		if parts[2] == "/" {
			continue
		}

		cgroupRoot := "/sys/fs/cgroup"
		// Special case the unified mount on hybrid cgroup and named hierarchies.
		// This works on Fedora 31, but we should really parse the mounts to see
		// where the cgroup hierarchy is mounted.
		if parts[1] == "" && !unifiedMode {
			// If it is not using unified mode, the cgroup v2 hierarchy is
			// usually mounted under /sys/fs/cgroup/unified
			cgroupRoot = filepath.Join(cgroupRoot, "unified")
		} else if parts[1] != "" {
			// Assume the controller is mounted at /sys/fs/cgroup/$CONTROLLER.
			controller := strings.TrimPrefix(parts[1], "name=")
			cgroupRoot = filepath.Join(cgroupRoot, controller)
		}

		processes, err := ioutil.ReadFile(filepath.Join(cgroupRoot, parts[2], "cgroup.procs"))
		if err != nil {
			return err
		}

		newCgroup := filepath.Join(cgroupRoot, parts[2], subtree)
		if err := os.Mkdir(newCgroup, 0755); err != nil {
			return err
		}

		f, err := os.OpenFile(filepath.Join(newCgroup, "cgroup.procs"), os.O_RDWR, 0755)
		if err != nil {
			return err
		}
		defer f.Close()

		for _, pid := range bytes.Split(processes, []byte("\n")) {
			if _, err := f.Write(pid); err != nil {
				logrus.Warnf("Cannot move process %s to cgroup %q", pid, newCgroup)
			}
		}
	}
	return nil
}

func newProp(name string, units interface{}) systemdDbus.Property {
	return systemdDbus.Property{
		Name:  name,
		Value: dbus.MakeVariant(units),
	}
}
