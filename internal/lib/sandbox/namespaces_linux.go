// +build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	nspkg "github.com/containernetworking/plugins/pkg/ns"
	"github.com/cri-o/cri-o/pkg/config"
	"github.com/cri-o/cri-o/utils"
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

// Creates a new persistent namespace and returns an object
// representing that namespace, without switching to it
func pinNamespaces(nsTypes []NSType, cfg *config.Config) ([]NamespaceIface, error) {
	typeToArg := map[NSType]string{
		IPCNS:  "-i",
		UTSNS:  "-u",
		USERNS: "-U",
		NETNS:  "-n",
	}

	pinnedNamespace := uuid.New().String()
	pinnsArgs := []string{
		"-d", cfg.NamespacesDir,
		"-f", pinnedNamespace,
	}
	type namespaceInfo struct {
		path   string
		nsType NSType
	}

	mountedNamespaces := make([]namespaceInfo, 0, len(nsTypes))
	for _, nsType := range nsTypes {
		arg, ok := typeToArg[nsType]
		if !ok {
			return nil, errors.Errorf("Invalid namespace type: %s", nsType)
		}
		pinnsArgs = append(pinnsArgs, arg)
		mountedNamespaces = append(mountedNamespaces, namespaceInfo{
			path:   filepath.Join(cfg.NamespacesDir, fmt.Sprintf("%sns", string(nsType)), pinnedNamespace),
			nsType: nsType,
		})
	}
	pinns := cfg.PinnsPath

	logrus.Debugf("calling pinns with %v", pinnsArgs)
	output, err := exec.Command(pinns, pinnsArgs...).Output()
	if len(output) != 0 {
		logrus.Debugf("pinns output: %s", string(output))
	}
	if err != nil {
		// cleanup after ourselves
		failedUmounts := make([]string, 0)
		for _, info := range mountedNamespaces {
			if unmountErr := unix.Unmount(info.path, unix.MNT_DETACH); unmountErr != nil {
				failedUmounts = append(failedUmounts, info.path)
			}
		}
		if len(failedUmounts) != 0 {
			return nil, fmt.Errorf("failed to cleanup %v after pinns failure %s %v", failedUmounts, output, err)
		}
		return nil, fmt.Errorf("failed to pin namespaces %v: %s %v", nsTypes, output, err)
	}

	returnedNamespaces := make([]NamespaceIface, 0)
	for _, info := range mountedNamespaces {
		ret, err := nspkg.GetNS(info.path)
		if err != nil {
			return nil, err
		}

		returnedNamespaces = append(returnedNamespaces, &Namespace{
			ns:     ret.(NS),
			nsType: info.nsType,
			nsPath: info.path,
		})
	}
	return returnedNamespaces, nil
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

	if err := utils.Unmount(n.Path()); err != nil {
		return errors.Wrapf(err, "failed to umount namespace path")
	}

	if n.Path() != "" {
		if err := os.RemoveAll(n.Path()); err != nil {
			return errors.Wrapf(err, "failed to remove namespace path")
		}
	}

	return nil
}
