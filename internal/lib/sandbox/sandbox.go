package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cri-o/cri-o/internal/oci"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/fields"
	pb "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/kubernetes/pkg/kubelet/dockershim/network/hostport"
)

// DevShmPath is the default system wide shared memory path
const DevShmPath = "/dev/shm"

var (
	sbStoppedFilename        = "stopped"
	sbNetworkStoppedFilename = "network-stopped"
)

// Sandbox contains data surrounding kubernetes sandboxes on the server
type Sandbox struct {
	portMappings []*hostport.PortMapping
	createdAt    time.Time
	id           string
	namespace    string
	// OCI pod name (eg "<namespace>-<name>-<attempt>")
	name string
	// Kubernetes pod name (eg, "<name>")
	kubeName       string
	logDir         string
	containers     oci.ContainerStorer
	processLabel   string
	mountLabel     string
	netns          NamespaceIface
	ipcns          NamespaceIface
	pidns          NamespaceIface
	utsns          NamespaceIface
	userns         NamespaceIface
	shmPath        string
	cgroupParent   string
	runtimeHandler string
	resolvPath     string
	hostnamePath   string
	hostname       string
	// ipv4 or ipv6 cache
	ips                []string
	seccompProfilePath string
	labels             fields.Set
	annotations        map[string]string
	infraContainer     *oci.Container
	metadata           *pb.PodSandboxMetadata
	nsOpts             *pb.NamespaceOption
	stopMutex          sync.RWMutex
	created            bool
	stopped            bool
	networkStopped     bool
	privileged         bool
	hostNetwork        bool
	usernsMode         string
}

// DefaultShmSize is the default shm size
const DefaultShmSize = 64 * 1024 * 1024

// ErrIDEmpty is the error returned when the id of the sandbox is empty
var ErrIDEmpty = errors.New("PodSandboxId should not be empty")

// New creates and populates a new pod sandbox
// New sandboxes have no containers, no infra container, and no network namespaces associated with them
// An infra container must be attached before the sandbox is added to the state
func New(id, namespace, name, kubeName, logDir string, labels, annotations map[string]string, processLabel, mountLabel string, metadata *pb.PodSandboxMetadata, shmPath, cgroupParent string, privileged bool, runtimeHandler, resolvPath, hostname string, portMappings []*hostport.PortMapping, hostNetwork bool, createdAt time.Time, usernsMode string) (*Sandbox, error) {
	sb := new(Sandbox)
	sb.id = id
	sb.namespace = namespace
	sb.name = name
	sb.kubeName = kubeName
	sb.logDir = logDir
	sb.labels = labels
	sb.annotations = annotations
	sb.containers = oci.NewMemoryStore()
	sb.processLabel = processLabel
	sb.mountLabel = mountLabel
	sb.metadata = metadata
	sb.shmPath = shmPath
	sb.cgroupParent = cgroupParent
	sb.privileged = privileged
	sb.runtimeHandler = runtimeHandler
	sb.resolvPath = resolvPath
	sb.hostname = hostname
	sb.portMappings = portMappings
	sb.createdAt = createdAt
	sb.hostNetwork = hostNetwork
	sb.usernsMode = usernsMode

	return sb, nil
}

func (s *Sandbox) CreatedAt() time.Time {
	return s.createdAt
}

// SetSeccompProfilePath sets the seccomp profile path
func (s *Sandbox) SetSeccompProfilePath(pp string) {
	s.seccompProfilePath = pp
}

// SeccompProfilePath returns the seccomp profile path
func (s *Sandbox) SeccompProfilePath() string {
	return s.seccompProfilePath
}

// AddIPs stores the ip in the sandbox
func (s *Sandbox) AddIPs(ips []string) {
	s.ips = ips
}

// SetNamespaceOptions sets whether the pod is running using host network
func (s *Sandbox) SetNamespaceOptions(nsOpts *pb.NamespaceOption) {
	s.nsOpts = nsOpts
}

// NamespaceOptions returns the namespace options for the sandbox
func (s *Sandbox) NamespaceOptions() *pb.NamespaceOption {
	return s.nsOpts
}

// StopMutex returns the mutex to use when stopping the sandbox
func (s *Sandbox) StopMutex() *sync.RWMutex {
	return &s.stopMutex
}

// IPs returns the ip of the sandbox
func (s *Sandbox) IPs() []string {
	return s.ips
}

// ID returns the id of the sandbox
func (s *Sandbox) ID() string {
	return s.id
}

// UsernsMode returns the mode for setting the user namespace, if any.
func (s *Sandbox) UsernsMode() string {
	return s.usernsMode
}

// Namespace returns the namespace for the sandbox
func (s *Sandbox) Namespace() string {
	return s.namespace
}

// Name returns the name of the sandbox
func (s *Sandbox) Name() string {
	return s.name
}

// KubeName returns the kubernetes name for the sandbox
func (s *Sandbox) KubeName() string {
	return s.kubeName
}

// LogDir returns the location of the logging directory for the sandbox
func (s *Sandbox) LogDir() string {
	return s.logDir
}

// Labels returns the labels associated with the sandbox
func (s *Sandbox) Labels() fields.Set {
	return s.labels
}

// Annotations returns a list of annotations for the sandbox
func (s *Sandbox) Annotations() map[string]string {
	return s.annotations
}

// Containers returns the ContainerStorer that contains information on all
// of the containers in the sandbox
func (s *Sandbox) Containers() oci.ContainerStorer {
	return s.containers
}

// ProcessLabel returns the process label for the sandbox
func (s *Sandbox) ProcessLabel() string {
	return s.processLabel
}

// MountLabel returns the mount label for the sandbox
func (s *Sandbox) MountLabel() string {
	return s.mountLabel
}

// Metadata returns a set of metadata about the sandbox
func (s *Sandbox) Metadata() *pb.PodSandboxMetadata {
	return s.metadata
}

// ShmPath returns the shm path of the sandbox
func (s *Sandbox) ShmPath() string {
	return s.shmPath
}

// CgroupParent returns the cgroup parent of the sandbox
func (s *Sandbox) CgroupParent() string {
	return s.cgroupParent
}

// Privileged returns whether or not the containers in the sandbox are
// privileged containers
func (s *Sandbox) Privileged() bool {
	return s.privileged
}

// RuntimeHandler returns the name of the runtime handler that should be
// picked from the list of runtimes. The name must match the key from the
// map of runtimes.
func (s *Sandbox) RuntimeHandler() string {
	return s.runtimeHandler
}

// HostNetwork returns whether the sandbox runs in the host network namespace
func (s *Sandbox) HostNetwork() bool {
	return s.hostNetwork
}

// ResolvPath returns the resolv path for the sandbox
func (s *Sandbox) ResolvPath() string {
	return s.resolvPath
}

// AddHostnamePath adds the hostname path to the sandbox
func (s *Sandbox) AddHostnamePath(hostname string) {
	s.hostnamePath = hostname
}

// HostnamePath retrieves the hostname path from a sandbox
func (s *Sandbox) HostnamePath() string {
	return s.hostnamePath
}

// Hostname returns the hostname of the sandbox
func (s *Sandbox) Hostname() string {
	return s.hostname
}

// PortMappings returns a list of port mappings between the host and the sandbox
func (s *Sandbox) PortMappings() []*hostport.PortMapping {
	return s.portMappings
}

// AddContainer adds a container to the sandbox
func (s *Sandbox) AddContainer(c *oci.Container) {
	s.containers.Add(c.Name(), c)
}

// GetContainer retrieves a container from the sandbox
func (s *Sandbox) GetContainer(name string) *oci.Container {
	return s.containers.Get(name)
}

// RemoveContainer deletes a container from the sandbox
func (s *Sandbox) RemoveContainer(c *oci.Container) {
	s.containers.Delete(c.Name())
}

// SetInfraContainer sets the infrastructure container of a sandbox
// Attempts to set the infrastructure container after one is already present will throw an error
func (s *Sandbox) SetInfraContainer(infraCtr *oci.Container) error {
	if s.infraContainer != nil {
		return fmt.Errorf("sandbox already has an infra container")
	} else if infraCtr == nil {
		return fmt.Errorf("must provide non-nil infra container")
	}

	s.infraContainer = infraCtr

	return nil
}

// InfraContainer returns the infrastructure container for the sandbox
func (s *Sandbox) InfraContainer() *oci.Container {
	return s.infraContainer
}

// RemoveInfraContainer removes the infrastructure container of a sandbox
func (s *Sandbox) RemoveInfraContainer() {
	s.infraContainer = nil
}

// SetStopped sets the sandbox state to stopped.
// This should be set after a stop operation succeeds
// so that subsequent stops can return fast.
// if createFile is true, it also creates a "stopped" file in the infra container's persistent dir
// this is used to track the sandbox is stopped over reboots
func (s *Sandbox) SetStopped(createFile bool) {
	if s.stopped {
		return
	}
	s.stopped = true
	if createFile {
		if err := s.createFileInInfraDir(sbStoppedFilename); err != nil {
			logrus.Errorf("failed to create stopped file in container state. Restore may fail: %v", err)
		}
	}
}

// Stopped returns whether the sandbox state has been
// set to stopped.
func (s *Sandbox) Stopped() bool {
	return s.stopped
}

// SetCreated sets the created status of sandbox to true
func (s *Sandbox) SetCreated() {
	s.created = true
}

// NetworkStopped returns whether the network has been stopped
func (s *Sandbox) NetworkStopped() bool {
	return s.networkStopped
}

// SetNetworkStopped sets the sandbox network state as stopped
// This should be set after a network stop operation succeeds,
// so we don't double stop the network
// if createFile is true, it creates a "network-stopped" file
// in the infra container's persistent dir
// this is used to track the network is stopped over reboots
// returns an error if an error occurred when creating the network-stopped file
func (s *Sandbox) SetNetworkStopped(createFile bool) error {
	if s.networkStopped {
		return nil
	}
	s.networkStopped = true
	if createFile {
		if err := s.createFileInInfraDir(sbNetworkStoppedFilename); err != nil {
			return fmt.Errorf("failed to create state file in container directory. Restores may fail: %v", err)
		}
	}
	return nil
}

func (s *Sandbox) createFileInInfraDir(filename string) error {
	infra := s.InfraContainer()
	f, err := os.Create(filepath.Join(infra.Dir(), filename))
	if err == nil {
		f.Close()
	}
	return err
}

func (s *Sandbox) RestoreStopped() {
	if s.fileExistsInInfraDir(sbStoppedFilename) {
		s.stopped = true
	}
	if s.fileExistsInInfraDir(sbNetworkStoppedFilename) {
		s.networkStopped = true
	}
}

func (s *Sandbox) fileExistsInInfraDir(filename string) bool {
	infra := s.InfraContainer()
	infraFilePath := filepath.Join(infra.Dir(), filename)
	if _, err := os.Stat(infraFilePath); err != nil {
		if !os.IsNotExist(err) {
			logrus.Warnf("error checking if %s exists: %v", infraFilePath, err)
		}
		return false
	}
	return true
}

// Created returns the created status of sandbox
func (s *Sandbox) Created() bool {
	return s.created
}

// Ready returns whether the sandbox should be marked as ready to the kubelet
// if there is no infra container, it is always considered ready
// takeLock should be set if we need to take the lock to get the infra container's state
func (s *Sandbox) Ready(takeLock bool) bool {
	podInfraContainer := s.InfraContainer()
	if podInfraContainer == nil {
		// Assume the sandbox is ready, unless it has an infra container that
		// isn't running
		return true
	}
	var cState *oci.ContainerState
	if takeLock {
		cState = podInfraContainer.State()
	} else {
		cState = podInfraContainer.StateNoLock()
	}

	return cState.Status == oci.ContainerStateRunning
}

// UnmountShm removes the shared memory mount for the sandbox and returns an
// error if any failure occurs.
func (s *Sandbox) UnmountShm() error {
	fp := s.ShmPath()
	if fp == DevShmPath {
		return nil
	}

	// try to unmount, ignoring "not mounted" (EINVAL) error and
	// "already unmounted" (ENOENT) error
	if err := unix.Unmount(fp, unix.MNT_DETACH); err != nil && err != unix.EINVAL && err != unix.ENOENT {
		return errors.Wrapf(err, "unable to unmount %s", fp)
	}

	return nil
}
