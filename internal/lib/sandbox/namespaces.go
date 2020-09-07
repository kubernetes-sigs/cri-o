package sandbox

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/containers/storage/pkg/idtools"
	"github.com/cri-o/cri-o/internal/oci"
	"github.com/cri-o/cri-o/pkg/config"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// NSType is an abstraction about available namespace types
type NSType string

const (
	NETNS         NSType = "net"
	IPCNS         NSType = "ipc"
	UTSNS         NSType = "uts"
	USERNS        NSType = "user"
	PIDNS         NSType = "pid"
	numNamespaces        = 4
)

var ErrNamespaceNotManaged = errors.New("sandbox namespace not managed")

// NamespaceIface provides a generic namespace interface
type NamespaceIface interface {
	// Close closes this network namespace
	Close() error

	// Get returns the native Namespace
	Get() *Namespace

	// Initialize does the necessary setup
	Initialize() NamespaceIface

	// Initialized returns true if already initialized
	Initialized() bool

	// Remove ensures this network namespace handle is closed and removed
	Remove() error

	// Path returns the bind mount path of the namespace
	Path() string

	// Type returns the namespace type (net, ipc or uts)
	Type() NSType
}

// ManagedNamespace is a structure that holds all the necessary information a caller would
// need for a sandbox managed namespace
// Where NamespaceIface does hold similar information, ManagedNamespace exists to allow this library
// to not return data not necessarily in a NamespaceIface (for instance, when a namespace is not managed
// by CRI-O, but instead is based off of the infra pid)
type ManagedNamespace struct {
	nsPath string
	nsType NSType
}

// Type returns the namespace type
func (m *ManagedNamespace) Type() NSType {
	return m.nsType
}

// Type returns the namespace path
func (m *ManagedNamespace) Path() string {
	return m.nsPath
}

// CreateManagedNamespaces calls pinnsPath on all the managed namespaces for the sandbox.
// It returns a slice of ManagedNamespaces it created.
func (s *Sandbox) CreateManagedNamespaces(managedNamespaces []NSType, idMappings *idtools.IDMappings, cfg *config.Config) ([]*ManagedNamespace, error) {
	return s.CreateNamespacesWithFunc(managedNamespaces, idMappings, cfg, pinNamespaces)
}

// CreateNamespacesWithFunc is mainly added for testing purposes. There's no point in actually calling the pinns binary
// in unit tests, so this function allows the actual pin func to be abstracted out. Every other caller should use CreateManagedNamespaces
func (s *Sandbox) CreateNamespacesWithFunc(managedNamespaces []NSType, idMappings *idtools.IDMappings, cfg *config.Config, pinFunc func([]NSType, *config.Config, *idtools.IDMappings) ([]NamespaceIface, error)) (mns []*ManagedNamespace, retErr error) {
	typesAndPaths := make([]*ManagedNamespace, 0, 4)
	if len(managedNamespaces) == 0 {
		return typesAndPaths, nil
	}

	namespaces, err := pinFunc(managedNamespaces, cfg, idMappings)
	if err != nil {
		return nil, err
	}

	for _, namespace := range namespaces {
		namespaceIface := namespace.Initialize()
		defer func() {
			if retErr != nil {
				if err1 := namespaceIface.Remove(); err1 != nil {
					logrus.Warnf("removing namespace interface returned: %v", err1)
				}
			}
		}()

		switch namespace.Type() {
		case NETNS:
			s.netns = namespaceIface
			typesAndPaths = append(typesAndPaths, &ManagedNamespace{
				nsType: NETNS,
				nsPath: namespace.Path(),
			})
		case IPCNS:
			s.ipcns = namespaceIface
			typesAndPaths = append(typesAndPaths, &ManagedNamespace{
				nsType: IPCNS,
				nsPath: namespace.Path(),
			})
		case UTSNS:
			s.utsns = namespaceIface
			typesAndPaths = append(typesAndPaths, &ManagedNamespace{
				nsType: UTSNS,
				nsPath: namespace.Path(),
			})
		case USERNS:
			if idMappings != nil {
				s.userns = namespaceIface
				typesAndPaths = append(typesAndPaths, &ManagedNamespace{
					nsType: USERNS,
					nsPath: namespace.Path(),
				})
			}
		default:
			// This should never happen
			err = errors.New("Invalid namespace type")
			return typesAndPaths, err
		}
	}

	return typesAndPaths, nil
}

// NamespacePaths returns all the paths of the namespaces of the sandbox. If a namespace is not
// managed by the sandbox, the namespace of the infra container will be returned.
// It returns a slice of ManagedNamespaces
func (s *Sandbox) NamespacePaths() []*ManagedNamespace {
	pid := infraPid(s.InfraContainer())

	typesAndPaths := make([]*ManagedNamespace, 0, numNamespaces)

	if ipc := nsPathGivenInfraPid(s.ipcns, IPCNS, pid); ipc != "" {
		typesAndPaths = append(typesAndPaths, &ManagedNamespace{
			nsType: IPCNS,
			nsPath: ipc,
		})
	}
	if net := nsPathGivenInfraPid(s.netns, NETNS, pid); net != "" {
		typesAndPaths = append(typesAndPaths, &ManagedNamespace{
			nsType: NETNS,
			nsPath: net,
		})
	}
	if uts := nsPathGivenInfraPid(s.utsns, UTSNS, pid); uts != "" {
		typesAndPaths = append(typesAndPaths, &ManagedNamespace{
			nsType: UTSNS,
			nsPath: uts,
		})
	}
	if user := nsPathGivenInfraPid(s.userns, USERNS, pid); user != "" {
		typesAndPaths = append(typesAndPaths, &ManagedNamespace{
			nsType: USERNS,
			nsPath: user,
		})
	}
	return typesAndPaths
}

// RemoveManagedNamespaces cleans up after managing the namespaces. It removes all of the namespaces
// and the parent directory in which they lived.
func (s *Sandbox) RemoveManagedNamespaces() error {
	errs := make([]error, 0)

	// use a map as a set to delete each parent directory just once
	if s.utsns != nil {
		if err := s.utsns.Remove(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.ipcns != nil {
		if err := s.ipcns.Remove(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.netns != nil {
		if err := s.netns.Remove(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.pidns != nil {
		if err := s.pidns.Remove(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.userns != nil {
		if err := s.userns.Remove(); err != nil {
			errs = append(errs, err)
		}
	}

	var err error
	if len(errs) != 0 {
		err = errors.Errorf("Removing namespaces encountered the following errors %v", errs)
	}
	return err
}

// NetNs specific functions

// NetNsPath returns the path to the network namespace of the sandbox.
// If the sandbox uses the host namespace, the empty string is returned
func (s *Sandbox) NetNsPath() string {
	return s.nsPath(s.netns, NETNS)
}

// NetNsJoin attempts to join the sandbox to an existing network namespace
// This will fail if the sandbox is already part of a network namespace
func (s *Sandbox) NetNsJoin(nspath string) error {
	ns, err := nsJoin(nspath, NETNS, s.netns)
	if err != nil {
		return err
	}
	s.netns = ns
	return nil
}

// IpcNs specific functions

// IpcNsPath returns the path to the network namespace of the sandbox.
// If the sandbox uses the host namespace, the empty string is returned
func (s *Sandbox) IpcNsPath() string {
	return s.nsPath(s.ipcns, IPCNS)
}

// IpcNsJoin attempts to join the sandbox to an existing IPC namespace
// This will fail if the sandbox is already part of a IPC namespace
func (s *Sandbox) IpcNsJoin(nspath string) error {
	ns, err := nsJoin(nspath, IPCNS, s.ipcns)
	if err != nil {
		return err
	}
	s.ipcns = ns
	return err
}

// UtsNs specific functions

// UtsNsPath returns the path to the network namespace of the sandbox.
// If the sandbox uses the host namespace, the empty string is returned
func (s *Sandbox) UtsNsPath() string {
	return s.nsPath(s.utsns, UTSNS)
}

// UtsNsJoin attempts to join the sandbox to an existing UTS namespace
// This will fail if the sandbox is already part of a UTS namespace
func (s *Sandbox) UtsNsJoin(nspath string) error {
	ns, err := nsJoin(nspath, UTSNS, s.utsns)
	if err != nil {
		return err
	}
	s.utsns = ns
	return err
}

// UserNs specific functions

// UserNsPath returns the path to the user namespace of the sandbox.
// If the sandbox uses the host namespace, the empty string is returned
func (s *Sandbox) UserNsPath() string {
	return s.nsPath(s.userns, USERNS)
}

// UserNsJoin attempts to join the sandbox to an existing User namespace
// This will fail if the sandbox is already part of a User namespace
func (s *Sandbox) UserNsJoin(nspath string) error {
	ns, err := nsJoin(nspath, USERNS, s.userns)
	if err != nil {
		return err
	}
	s.userns = ns
	return err
}

// PidNs specific functions
// CreateManagedPidNamespace creates a managed pid namespace
// it is separate from the other namespaces because pid namespaces need special handling
// We cannot tell the runtime: "here is your pid namespace!", because it gets created
// when the container is created and it would would be a bother having conmon (the parent of the container process)
// unshare and then do the bind mount
// instead, we mount the sandbox's pid namespace after the infra container is created
// and from then on refer to it for each container create
// thus, this should be called after the infra container has been created in the runtime
func (s *Sandbox) CreateManagedPidNamespace(cfg *config.Config) error {
	return s.CreateManagedPidNamespaceWithFunc(cfg, pinPidNamespace)
}

func (s *Sandbox) CreateManagedPidNamespaceWithFunc(cfg *config.Config, pinFunc func(*config.Config, string) (NamespaceIface, error)) (retErr error) {
	if cfg == nil {
		return errors.New("cannot create a managed pid namespace with a nil config")
	}

	podPidnsProc := s.nsPath(nil, PIDNS)
	// pid must have stopped or be incorrect, report error
	if podPidnsProc == "" {
		return errors.Errorf("proc entry for sandbox %s is gone; pid not created or stopped", s.id)
	}

	// since this is the first time the sandbox's pidns
	// has been requested, we need to bind mount it to a new spot
	// this will allow us to pay less attention to the infra pid for future calls
	pidnsIface, err := pinFunc(cfg, podPidnsProc)
	if err != nil {
		return err
	}

	// if we fail the below write, we need to clean up the mount we created
	defer func() {
		if retErr != nil {
			if err := pidnsIface.Remove(); err != nil {
				logrus.Errorf("failed to clean up pidns after we failed to create it: %v", err)
			}
		}
	}()

	if err := s.writePidNsLocation(pidnsIface.Path()); err != nil {
		return errors.Wrapf(err, "failed to write persistent location of pid")
	}
	s.pidns = pidnsIface

	return nil
}

// PidNsPath returns the path to the pid namespace of the sandbox.
// If the sandbox uses the host namespace, the empty string is returned
// We need the cri-o config to make sure we get the namespace
// mounted in the correct spot.
func (s *Sandbox) PidNsPath() string {
	return s.nsPath(s.pidns, PIDNS)
}

// Attempts to join the pid namespace, whose location is saved in
// the infra container's run dir.
// if the location cannot be found (cri-o was restarted from not managing namespaces
// to managing namespaces), an error ErrNamespaceNotManaged is returned
func (s *Sandbox) PidNsJoin() error {
	path, err := s.pidNsLocation()
	if err != nil {
		return err
	}
	ns, err := nsJoin(path, PIDNS, s.pidns)
	if err != nil {
		return err
	}
	s.pidns = ns
	return err
}

func (s *Sandbox) writePidNsLocation(path string) error {
	if err := ioutil.WriteFile(s.pidNsLocationFile(), []byte(path), 0o644); err != nil {
		return err
	}
	return nil
}

func (s *Sandbox) pidNsLocation() (string, error) {
	contents, err := ioutil.ReadFile(s.pidNsLocationFile())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrNamespaceNotManaged
		}
		return "", err
	}
	return string(contents), nil
}

func (s *Sandbox) pidNsLocationFile() string {
	infra := s.InfraContainer()
	if infra == nil {
		return ""
	}
	return filepath.Join(infra.Dir(), "pid-location")
}

// nsJoin checks if the current iface is nil, and if so gets the namespace at nsPath
func nsJoin(nsPath string, nsType NSType, currentIface NamespaceIface) (NamespaceIface, error) {
	if currentIface != nil {
		return currentIface, fmt.Errorf("sandbox already has a %s namespace, cannot join another", nsType)
	}

	return getNamespace(nsPath)
}

// nsPath returns the path to a namespace of the sandbox.
// If the sandbox uses the host namespace, nil is returned
func (s *Sandbox) nsPath(ns NamespaceIface, nsType NSType) string {
	return nsPathGivenInfraPid(ns, nsType, infraPid(s.InfraContainer()))
}

// infraPid returns the pid of the passed in infra container
// if the infra container is nil, pid is returned negative
func infraPid(infra *oci.Container) int {
	pid := -1
	if infra != nil {
		var err error
		pid, err = infra.Pid()
		// There are some cases where ErrNotInitialized is expected.
		// For instance, when we're creating a pod sandbox while managing namespace lifecycle,
		// we create the network stack before we create the infra container.
		// Since we're pinning namespaces, we already have the namespace we need.
		// Later users of this pid will either find we have a valid pinned namespace (which we will in this case),
		// or find we have an invalid /proc entry (a negative pid).
		// Thus, we don't need to error here if the pid is not initialized
		if err != nil && err != oci.ErrNotInitialized {
			logrus.Errorf("pid for infra container %s not found: %v", infra.ID(), err)
		}
	}
	return pid
}

// nsPathGivenInfraPid allows callers to cache the infra pid, rather than
// calling a container.State() in batch operations
func nsPathGivenInfraPid(ns NamespaceIface, nsType NSType, infraPid int) string {
	// caller is responsible for checking if infraContainer
	// is valid. If not, infraPid should be less than or equal to 0
	if ns == nil || ns.Get() == nil {
		if infraPid > 0 {
			return infraNsPath(nsType, infraPid)
		}
		return ""
	}

	return ns.Path()
}

// infraNsPath returns the namespace path of type nsType for infra
// with pid infraContainerPid
func infraNsPath(nsType NSType, infraContainerPid int) string {
	// verify nsPath exists on the host. This will prevent us from fatally erroring
	// on network tear down if the path doesn't exist
	// Technically, this is pretty racy, but so is every check using the infra container PID.
	// Without managing the namespaces, this is the best we can do
	nsPath := fmt.Sprintf("/proc/%d/ns/%s", infraContainerPid, nsType)
	if _, err := os.Stat(nsPath); err != nil {
		return ""
	}
	return nsPath
}
