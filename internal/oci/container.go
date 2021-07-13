package oci

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	metadata "github.com/checkpoint-restore/checkpointctl/lib"
	"github.com/containers/podman/v3/pkg/cgroups"
	"github.com/containers/storage/pkg/idtools"
	ann "github.com/cri-o/cri-o/pkg/annotations"
	json "github.com/json-iterator/go"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/kubernetes/pkg/kubelet/types"
)

const (
	defaultStopSignalInt = 15
	// the following values can be verified here: https://man7.org/linux/man-pages/man5/proc.5.html
	// the 22nd field is the process starttime
	statStartTimeLocation = 22
	// The 2nd field is the command, wrapped by ()
	statCommField = 2
)

var (
	defaultStopSignal   = strconv.Itoa(defaultStopSignalInt)
	ErrContainerStopped = errors.New("container is already stopped")
	ErrNotFound         = errors.New("container process not found")
	ErrNotInitialized   = errors.New("container PID not initialized")
)

// Container represents a runtime container.
type Container struct {
	volumes        []ContainerVolume
	id             string
	name           string
	logPath        string
	image          string
	sandbox        string
	runtimeHandler string
	// this is the /var/run/storage/... directory, erased on reboot
	bundlePath string
	// this is the /var/lib/storage/... directory
	dir                string
	stopSignal         string
	imageName          string
	imageRef           string
	mountPoint         string
	seccompProfilePath string
	conmonCgroupfsPath string
	labels             fields.Set
	annotations        fields.Set
	crioAnnotations    fields.Set
	state              *ContainerState
	metadata           *Metadata
	opLock             sync.RWMutex
	spec               *specs.Spec
	idMappings         *idtools.IDMappings
	terminal           bool
	stdin              bool
	stdinOnce          bool
	created            bool
	spoofed            bool
	stopping           bool
	stopTimeoutChan    chan time.Duration
	stoppedChan        chan struct{}
	stopLock           sync.Mutex
}

// Metadata holds all necessary information for building the container name.
// The container runtime is encouraged to expose the metadata in its user
// interface for better user experience.
type Metadata struct {
	// Name of the container.
	Name string `json:"name,omitempty"`

	// Attempt number of creating the container.
	Attempt uint32 `json:"attempt,omitempty"`
}

// ContainerVolume is a bind mount for the container.
type ContainerVolume struct {
	ContainerPath string `json:"container_path"`
	HostPath      string `json:"host_path"`
	Readonly      bool   `json:"readonly"`
}

// ContainerState represents the status of a container.
type ContainerState struct {
	specs.State
	Created   time.Time `json:"created"`
	Started   time.Time `json:"started,omitempty"`
	Finished  time.Time `json:"finished,omitempty"`
	ExitCode  *int32    `json:"exitCode,omitempty"`
	OOMKilled bool      `json:"oomKilled,omitempty"`
	Error     string    `json:"error,omitempty"`
	InitPid   int       `json:"initPid,omitempty"`
	// The unix start time of the container's init PID.
	// This is used to track whether the PID we have stored
	// is the same as the corresponding PID on the host.
	InitStartTime string `json:"initStartTime,omitempty"`
}

// NewContainer creates a container object.
func NewContainer(id, name, bundlePath, logPath string, labels, crioAnnotations, annotations map[string]string, image, imageName, imageRef string, md *Metadata, sandbox string, terminal, stdin, stdinOnce bool, runtimeHandler, dir string, created time.Time, stopSignal string) (*Container, error) {
	state := &ContainerState{}
	state.Created = created
	c := &Container{
		id:              id,
		name:            name,
		bundlePath:      bundlePath,
		logPath:         logPath,
		labels:          labels,
		sandbox:         sandbox,
		terminal:        terminal,
		stdin:           stdin,
		stdinOnce:       stdinOnce,
		runtimeHandler:  runtimeHandler,
		metadata:        md,
		annotations:     annotations,
		crioAnnotations: crioAnnotations,
		image:           image,
		imageName:       imageName,
		imageRef:        imageRef,
		dir:             dir,
		state:           state,
		stopSignal:      stopSignal,
		stopTimeoutChan: make(chan time.Duration, 1),
		stoppedChan:     make(chan struct{}, 1),
	}
	return c, nil
}

func NewSpoofedContainer(id, name string, labels map[string]string, sandbox string, created time.Time, dir string) *Container {
	state := &ContainerState{}
	state.Created = created
	state.Started = created
	c := &Container{
		id:      id,
		name:    name,
		labels:  labels,
		spoofed: true,
		state:   state,
		dir:     dir,
		sandbox: sandbox,
	}
	c.annotations = map[string]string{
		ann.SpoofedContainer: "true",
	}
	return c
}

// SetSpec loads the OCI spec in the container struct
func (c *Container) SetSpec(s *specs.Spec) {
	c.spec = s
}

// Spec returns a copy of the spec for the container
func (c *Container) Spec() specs.Spec {
	return *c.spec
}

// ConmonCgroupfsPath returns the path to conmon's cgroup. This is only set when
// cgroupfs is used as a cgroup manager.
func (c *Container) ConmonCgroupfsPath() string {
	return c.conmonCgroupfsPath
}

// GetStopSignal returns the container's own stop signal configured from the
// image configuration or the default one.
func (c *Container) GetStopSignal() string {
	if c.stopSignal == "" {
		return defaultStopSignal
	}
	signal := unix.SignalNum(strings.ToUpper(c.stopSignal))
	if signal == 0 {
		return defaultStopSignal
	}
	// return the stop signal in the form of its int converted to a string
	// i.e stop signal 34 is returned as "34" to avoid back and forth conversion
	return strconv.Itoa(int(signal))
}

// StopSignal returns the container's own stop signal configured from
// the image configuration or the default one.
func (c *Container) StopSignal() syscall.Signal {
	if c.stopSignal == "" {
		return defaultStopSignalInt
	}

	signal := unix.SignalNum(strings.ToUpper(c.stopSignal))
	if signal == 0 {
		return defaultStopSignalInt
	}
	return signal
}

// FromDisk restores container's state from disk
// Calls to FromDisk should always be preceded by call to Runtime.UpdateContainerStatus.
// This is because FromDisk() initializes the InitStartTime for the saved container state
// when CRI-O is being upgraded to a version that supports tracking PID,
// but does no verification the container is actually still running. If we assume the container
// is still running, we could incorrectly think a process with the same PID running on the host
// is our container. A call to `$runtime state` will protect us against this.
func (c *Container) FromDisk() error {
	jsonSource, err := os.Open(c.StatePath())
	if err != nil {
		return err
	}
	defer jsonSource.Close()

	dec := json.NewDecoder(jsonSource)
	tmpState := &ContainerState{}
	if err := dec.Decode(tmpState); err != nil {
		return err
	}

	// this is to handle the situation in which we're upgrading
	// versions of cri-o, and we didn't used to have this information in the state
	if tmpState.InitPid == 0 && tmpState.InitStartTime == "" && tmpState.Pid != 0 {
		if err := tmpState.SetInitPid(tmpState.Pid); err != nil {
			return err
		}
		logrus.Infof("PID information for container %s updated to %d %s", c.id, tmpState.InitPid, tmpState.InitStartTime)
	}
	c.state = tmpState
	return nil
}

// SetInitPid initializes the InitPid and InitStartTime for the container state
// given a PID.
// These values should be set once, and not changed again.
func (cstate *ContainerState) SetInitPid(pid int) error {
	if cstate.InitPid != 0 || cstate.InitStartTime != "" {
		return errors.Errorf("pid and start time already initialized: %d %s", cstate.InitPid, cstate.InitStartTime)
	}
	cstate.InitPid = pid
	startTime, err := getPidStartTime(pid)
	if err != nil {
		return err
	}
	cstate.InitStartTime = startTime
	return nil
}

// StatePath returns the containers state.json path
func (c *Container) StatePath() string {
	return filepath.Join(c.dir, "state.json")
}

// CreatedAt returns the container creation time
func (c *Container) CreatedAt() time.Time {
	return c.state.Created
}

// Name returns the name of the container.
func (c *Container) Name() string {
	return c.name
}

// ID returns the id of the container.
func (c *Container) ID() string {
	return c.id
}

// CleanupConmonCgroup cleans up conmon's group when using cgroupfs.
func (c *Container) CleanupConmonCgroup() {
	if c.spoofed {
		return
	}
	path := c.ConmonCgroupfsPath()
	if path == "" {
		return
	}
	cg, err := cgroups.Load(path)
	if err != nil {
		logrus.Infof("Error loading conmon cgroup of container %s: %v", c.ID(), err)
		return
	}
	if err := cg.Delete(); err != nil {
		logrus.Infof("Error deleting conmon cgroup of container %s: %v", c.ID(), err)
	}
}

// SetSeccompProfilePath sets the seccomp profile path
func (c *Container) SetSeccompProfilePath(pp string) {
	c.seccompProfilePath = pp
}

// SeccompProfilePath returns the seccomp profile path
func (c *Container) SeccompProfilePath() string {
	return c.seccompProfilePath
}

// BundlePath returns the bundlePath of the container.
func (c *Container) BundlePath() string {
	return c.bundlePath
}

// LogPath returns the log path of the container.
func (c *Container) LogPath() string {
	return c.logPath
}

// Labels returns the labels of the container.
func (c *Container) Labels() map[string]string {
	return c.labels
}

// Annotations returns the annotations of the container.
func (c *Container) Annotations() map[string]string {
	return c.annotations
}

// CrioAnnotations returns the crio annotations of the container.
func (c *Container) CrioAnnotations() map[string]string {
	return c.crioAnnotations
}

// Image returns the image of the container.
func (c *Container) Image() string {
	return c.image
}

// ImageName returns the image name of the container.
func (c *Container) ImageName() string {
	return c.imageName
}

// ImageRef returns the image ref of the container.
func (c *Container) ImageRef() string {
	return c.imageRef
}

// Sandbox returns the sandbox name of the container.
func (c *Container) Sandbox() string {
	return c.sandbox
}

// Dir returns the dir of the container
func (c *Container) Dir() string {
	return c.dir
}

// CheckpointPath returns the path to the directory containing the checkpoint
func (c *Container) CheckpointPath() string {
	// Podman uses 'bundlePath' as base directory for the checkpoint
	// CRI-O uses 'dir' instead of bundlePath as bundlePath seems to be
	// normally based on a tmpfs which does not survive a reboot. Also, as
	// the checkpoint contains all memory pages, it can be as large as the
	// available memory and writing that again to a tmpfs might lead to
	// problems. 'dir' seems to be based on /var
	return filepath.Join(c.dir, metadata.CheckpointDirectory)
}

// Metadata returns the metadata of the container.
func (c *Container) Metadata() *Metadata {
	return c.metadata
}

// State returns the state of the running container
func (c *Container) State() *ContainerState {
	c.opLock.RLock()
	defer c.opLock.RUnlock()
	return c.state
}

// StateNoLock returns the state of a container without using a lock.
func (c *Container) StateNoLock() *ContainerState {
	return c.state
}

// AddVolume adds a volume to list of container volumes.
func (c *Container) AddVolume(v ContainerVolume) {
	c.volumes = append(c.volumes, v)
}

// Volumes returns the list of container volumes.
func (c *Container) Volumes() []ContainerVolume {
	return c.volumes
}

// SetMountPoint sets the container mount point
func (c *Container) SetMountPoint(mp string) {
	c.mountPoint = mp
}

// MountPoint returns the container mount point
func (c *Container) MountPoint() string {
	return c.mountPoint
}

// SetIDMappings sets the ID/GID mappings used for the container
func (c *Container) SetIDMappings(mappings *idtools.IDMappings) {
	c.idMappings = mappings
}

// IDMappings returns the ID/GID mappings used for the container
func (c *Container) IDMappings() *idtools.IDMappings {
	return c.idMappings
}

// SetCreated sets the created flag to true once container is created
func (c *Container) SetCreated() {
	c.created = true
}

// Created returns whether the container was created successfully
func (c *Container) Created() bool {
	return c.created
}

// SetStartFailed sets the container state appropriately after a start failure
func (c *Container) SetStartFailed(err error) {
	c.opLock.Lock()
	defer c.opLock.Unlock()
	// adjust finished and started times
	c.state.Finished, c.state.Started = c.state.Created, c.state.Created
	if err != nil {
		c.state.Error = err.Error()
	}
}

// Description returns a description for the container
func (c *Container) Description() string {
	return fmt.Sprintf("%s/%s/%s", c.Labels()[types.KubernetesPodNamespaceLabel], c.Labels()[types.KubernetesPodNameLabel], c.Labels()[types.KubernetesContainerNameLabel])
}

// StdinOnce returns whether stdin once is set for the container.
func (c *Container) StdinOnce() bool {
	return c.stdinOnce
}

func (c *Container) exitFilePath() string {
	return filepath.Join(c.dir, "exit")
}

// IsAlive is a function that checks if a container's init PID exists.
// It is used to check a container state when we don't want a `$runtime state` call
func (c *Container) IsAlive() error {
	_, err := c.pid()
	return errors.Wrapf(err, "checking if PID of %s is running failed", c.id)
}

// Pid returns the container's init PID.
// It will fail if the saved PID no longer belongs to the container.
func (c *Container) Pid() (int, error) {
	c.opLock.Lock()
	defer c.opLock.Unlock()
	return c.pid()
}

// pid returns the container's init PID.
// It checks that we have an InitPid defined in the state, that PID can be found
// and it is the same process that was originally started by the runtime.
func (c *Container) pid() (int, error) {
	if c.state == nil {
		return 0, ErrNotInitialized
	}
	if c.state.InitPid <= 0 {
		return 0, ErrNotInitialized
	}

	// container has stopped (as pid is initialized but the runc state has overwritten it)
	if c.state.Pid == 0 {
		return 0, ErrNotFound
	}

	if err := c.verifyPid(); err != nil {
		return 0, err
	}
	if err := unix.Kill(c.state.InitPid, 0); err == unix.ESRCH {
		return 0, errors.Wrapf(err, "check whether %d is running", c.state.InitPid)
	}
	return c.state.InitPid, nil
}

// verifyPid checks that the start time for the process on the node is the same
// as the start time we saved after creating the container.
// This is the simplest way to verify we are operating on the container
// process, and haven't run into PID wrap.
func (c *Container) verifyPid() error {
	startTime, err := getPidStartTime(c.state.InitPid)
	if err != nil {
		return err
	}

	if startTime != c.state.InitStartTime {
		return errors.Errorf(
			"PID %d is running but has start time of %s, whereas the saved start time is %s. PID wrap may have occurred",
			c.state.InitPid, startTime, c.state.InitStartTime,
		)
	}
	return nil
}

// getPidStartTime reads the kernel's /proc entry for stime for PID.
// inspiration for this function came from https://github.com/containers/psgo/blob/master/internal/proc/stat.go
// some credit goes to the psgo authors
func getPidStartTime(pid int) (string, error) {
	return GetPidStartTimeFromFile(fmt.Sprintf("/proc/%d/stat", pid))
}

// GetPidStartTime reads a file as if it were a /proc/$pid/stat file, looking for stime for PID.
// It is abstracted out to allow for unit testing
func GetPidStartTimeFromFile(file string) (string, error) {
	data, err := ioutil.ReadFile(file)
	if err != nil {
		return "", errors.Wrapf(ErrNotFound, err.Error())
	}
	// The command (2nd field) can have spaces, but is wrapped in ()
	// first, trim it
	commEnd := bytes.LastIndexByte(data, ')')
	if commEnd == -1 {
		return "", errors.Wrapf(ErrNotFound, "unable to find ')' in stat file")
	}

	// start on the space after the command
	iter := commEnd + 1
	// for the number of fields between command and stime, trim the beginning word
	for field := 0; field < statStartTimeLocation-statCommField; field++ {
		// trim from the beginning to the character after the last space
		data = data[iter+1:]
		// find the next space
		iter = bytes.IndexByte(data, ' ')
		if iter == -1 {
			return "", errors.Wrapf(ErrNotFound, "invalid number of entries found in stat file %s: %d", file, field-1)
		}
	}

	// and return the startTime (not including the following space)
	return string(data[:iter]), nil
}

// ShouldBeStopped checks whether the container state is in a place
// where attempting to stop it makes sense
// a container is not stoppable if it's paused or stopped
// if it's paused, that's an error, and is reported as such
func (c *Container) ShouldBeStopped() error {
	switch c.state.Status {
	case ContainerStateStopped: // no-op
		return ErrContainerStopped
	case ContainerStatePaused:
		return errors.New("cannot stop paused container")
	}
	return nil
}

// Spoofed returns whether this container is spoofed.
// A container should be spoofed when it doesn't have to exist in the container runtime,
// but does need to exist in the storage. The main use of this is when an infra container
// is not needed, but sandbox metadata should be stored with a spoofed infra container.
func (c *Container) Spoofed() bool {
	return c.spoofed
}

// SetAsStopping marks a container as being stopped.
// If a stop is currently happening, it also sends the new timeout
// along the stopTimeoutChan, allowing the in-progress stop
// to stop faster, or ignore the new stop timeout.
func (c *Container) SetAsStopping(timeout int64) {
	// First, need to check if the container is already stopping
	c.stopLock.Lock()
	defer c.stopLock.Unlock()
	if c.stopping {
		// If so, we shouldn't wait forever on the opLock.
		// This can cause issues where the container stop gets DOSed by a very long
		// timeout, followed a shorter one coming in.
		// Instead, interrupt the other stop with this new one.
		select {
		case c.stopTimeoutChan <- time.Duration(timeout) * time.Second:
		case <-c.stoppedChan: // This case is to avoid waiting forever once another routine has finished.
			return
		}
	}
	// Regardless, set the container as actively stopping.
	c.stopping = true
}

// SetAsNotStopping unsets the stopping field indicating to new callers that the container
// is no longer actively stopping.
func (c *Container) SetAsNotStopping() {
	c.stopLock.Lock()
	c.stopping = false
	c.stopLock.Unlock()
}
