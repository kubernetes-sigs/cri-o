// +build linux

package sandbox

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	nspkg "github.com/containernetworking/plugins/pkg/ns"
	"github.com/containers/storage/pkg/idtools"
	"github.com/cri-o/cri-o/pkg/config"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// Namespace handles data pertaining to a namespace
type Namespace struct {
	sync.Mutex
	ns          NS
	closed      bool
	initialized bool
	nsType      NSType
	nsPath      string
}

// NS is a wrapper for the containernetworking plugin's NetNS interface
// It exists because while NetNS is specifically called such, it is really a generic
// namespace, and can be used for other namespaces
type NS interface {
	nspkg.NetNS
}

// Get returns the Namespace for a given NsIface
func (n *Namespace) Get() *Namespace {
	return n
}

// Initialized returns true if the Namespace is already initialized
func (n *Namespace) Initialized() bool {
	return n.initialized
}

// Initialize does the necessary setup for a Namespace
// It does not do the bind mounting and nspinning
func (n *Namespace) Initialize() NamespaceIface {
	n.closed = false
	n.initialized = true
	return n
}

func getMappingsForPinns(mappings []idtools.IDMap) string {
	g := new(bytes.Buffer)
	for _, m := range mappings {
		fmt.Fprintf(g, "%d-%d-%d@", m.ContainerID, m.HostID, m.Size)
	}
	return g.String()
}

type namespaceInfo struct {
	path   string
	nsType NSType
}

func newNamespaceInfo(nsDir, nsFile string, nsType NSType) *namespaceInfo {
	return &namespaceInfo{
		path:   filepath.Join(nsDir, fmt.Sprintf("%sns", string(nsType)), nsFile),
		nsType: nsType,
	}
}

func (info *namespaceInfo) toIface() (NamespaceIface, error) {
	ret, err := nspkg.GetNS(info.path)
	if err != nil {
		return nil, err
	}

	return &Namespace{
		ns:     ret.(NS),
		nsType: info.nsType,
		nsPath: info.path,
	}, nil
}

// Creates a new persistent namespace and returns an object
// representing that namespace, without switching to it
func pinNamespaces(nsTypes []NSType, cfg *config.Config, idMappings *idtools.IDMappings) ([]NamespaceIface, error) {
	typeToArg := map[NSType]string{
		IPCNS:  "-i",
		UTSNS:  "-u",
		USERNS: "-U",
		NETNS:  "-n",
	}

	numNSToPin := len(nsTypes)

	pinnedNamespace := uuid.New().String()
	pinnsArgs := []string{
		"-d", cfg.NamespacesDir,
		"-f", pinnedNamespace,
	}

	mountedNamespaces := make([]*namespaceInfo, 0, numNSToPin)

	var rootPair idtools.IDPair
	if idMappings != nil {
		rootPair = idMappings.RootPair()
	}

	for _, nsType := range nsTypes {
		arg, ok := typeToArg[nsType]
		if !ok {
			return nil, errors.Errorf("Invalid namespace type: %s", nsType)
		}
		pinnsArgs = append(pinnsArgs, arg)
		pinPath := filepath.Join(cfg.NamespacesDir, string(nsType)+"ns", pinnedNamespace)
		mountedNamespaces = append(mountedNamespaces, newNamespaceInfo(cfg.NamespacesDir, pinnedNamespace, nsType))
		if idMappings != nil {
			err := os.MkdirAll(filepath.Dir(pinPath), 0o755)
			if err != nil {
				return nil, err
			}
			f, err := os.Create(pinPath)
			if err != nil {
				return nil, err
			}
			f.Close()
			if err := os.Chown(pinPath, rootPair.UID, rootPair.GID); err != nil {
				return nil, err
			}
		}
	}

	if idMappings != nil {
		pinnsArgs = append(pinnsArgs,
			fmt.Sprintf("--uid-mapping=%s", getMappingsForPinns(idMappings.UIDs())),
			fmt.Sprintf("--gid-mapping=%s", getMappingsForPinns(idMappings.GIDs())))
	}

	logrus.Debugf("calling pinns with %v", pinnsArgs)
	output, err := exec.Command(cfg.PinnsPath, pinnsArgs...).CombinedOutput()
	if err != nil {
		logrus.Warnf("pinns %v failed: %s (%v)", pinnsArgs, string(output), err)
		// cleanup the mounts
		for _, info := range mountedNamespaces {
			if mErr := unix.Unmount(info.path, unix.MNT_DETACH); mErr != nil && mErr != unix.EINVAL {
				logrus.Warnf("failed to unmount %s: %v", info.path, mErr)
			}
		}

		return nil, fmt.Errorf("failed to pin namespaces %v: %s %v", nsTypes, output, err)
	}

	returnedNamespaces := make([]NamespaceIface, 0, numNSToPin)
	for _, info := range mountedNamespaces {
		iface, err := info.toIface()
		if err != nil {
			return nil, err
		}
		returnedNamespaces = append(returnedNamespaces, iface)
	}
	return returnedNamespaces, nil
}

// this function is heavily based on containernetworking ns package found at
// https://github.com/containernetworking/plugins/blob/5c3c17164270150467498a32c71436c7cd5501be/pkg/ns/ns.go#L140
// Credit goes to the CNI authors
func pinPidNamespace(cfg *config.Config, path string) (iface NamespaceIface, retErr error) {
	// verify the path we were passed is indeed a namespace
	if err := nspkg.IsNSorErr(path); err != nil {
		return nil, err
	}
	pinnedNamespace := uuid.New().String()
	namespaceInfo := newNamespaceInfo(cfg.NamespacesDir, pinnedNamespace, PIDNS)

	// ensure the parent directory is there
	if err := os.MkdirAll(filepath.Join(cfg.NamespacesDir, "pidns"), 0o755); err != nil {
		return nil, errors.Wrapf(err, "error creating parent pidns directory")
	}

	// now create an empty file
	f, err := os.Create(namespaceInfo.path)
	if err != nil {
		return nil, errors.Wrapf(err, "error creating pid namespace path")
	}
	f.Close()

	defer func() {
		if retErr != nil {
			if err2 := os.RemoveAll(namespaceInfo.path); err != nil {
				logrus.Errorf("failed to remove namespace path %s after failure to pin PID namespace: %v", namespaceInfo.path, err2)
			}
		}
	}()

	// bind mount the new netns from the pidns entry onto the mount point
	if err := unix.Mount(path, namespaceInfo.path, "none", unix.MS_BIND, ""); err != nil {
		return nil, errors.Wrapf(err, "error mounting pid namespace path")
	}
	defer func() {
		if retErr != nil {
			if err := unix.Unmount(namespaceInfo.path, unix.MNT_DETACH); err != nil && err != unix.EINVAL {
				logrus.Errorf("failed umount after failed to pin pid namespace: %v", err)
			}
		}
	}()

	return namespaceInfo.toIface()
}

// getNamespace takes a path, checks if it is a namespace, and if so
// returns a Namespace
func getNamespace(nsPath string) (*Namespace, error) {
	if err := nspkg.IsNSorErr(nsPath); err != nil {
		return nil, err
	}

	ns, err := nspkg.GetNS(nsPath)
	if err != nil {
		return nil, err
	}

	return &Namespace{ns: ns, closed: false, nsPath: nsPath}, nil
}

// Path returns the path of the namespace handle
func (n *Namespace) Path() string {
	if n == nil || n.ns == nil {
		return ""
	}
	return n.nsPath
}

// Type returns which namespace this structure represents
func (n *Namespace) Type() NSType {
	return n.nsType
}

// Close closes this namespace
func (n *Namespace) Close() error {
	if n == nil || n.ns == nil {
		return nil
	}
	return n.ns.Close()
}

// Remove ensures this namespace handle is closed and removed
func (n *Namespace) Remove() error {
	n.Lock()
	defer n.Unlock()

	if n.closed {
		// nsRemove() can be called multiple
		// times without returning an error.
		return nil
	}

	if err := n.Close(); err != nil {
		return err
	}

	n.closed = true

	fp := n.Path()
	if fp == "" {
		return nil
	}

	// try to unmount, ignoring "not mounted" (EINVAL) error
	if err := unix.Unmount(fp, unix.MNT_DETACH); err != nil && err != unix.EINVAL {
		return errors.Wrapf(err, "unable to unmount %s", fp)
	}
	return os.RemoveAll(fp)
}
