package oci

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cri-o/cri-o/pkg/config"
	"github.com/cri-o/cri-o/server/cri/types"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/net/context"

	"k8s.io/client-go/tools/remotecommand"
)

const (
	// ContainerStateCreated represents the created state of a container
	ContainerStateCreated = "created"
	// ContainerStatePaused represents the paused state of a container
	ContainerStatePaused = "paused"
	// ContainerStateRunning represents the running state of a container
	ContainerStateRunning = "running"
	// ContainerStateStopped represents the stopped state of a container
	ContainerStateStopped = "stopped"
	// ContainerCreateTimeout represents the value of container creating timeout
	ContainerCreateTimeout = 240 * time.Second

	// killContainerTimeout is the timeout that we wait for the container to
	// be SIGKILLed.
	killContainerTimeout = 2 * time.Minute
)

// Runtime is the generic structure holding both global and specific
// information about the runtime.
type Runtime struct {
	config              *config.Config
	runtimeImplMap      map[string]RuntimeImpl
	runtimeImplMapMutex sync.RWMutex
}

// RuntimeImpl is an interface used by the caller to interact with the
// container runtime. The purpose of this interface being to abstract
// implementations and their associated assumptions regarding the way to
// interact with containers. This will allow for new implementations of
// this interface, especially useful for the case of VM based container
// runtimes. Assumptions based on the fact that a container process runs
// on the host will be limited to the RuntimeOCI implementation.
type RuntimeImpl interface {
	CreateContainer(context.Context, *Container, string, bool) error
	StartContainer(context.Context, *Container) error
	ExecContainer(context.Context, *Container, []string, io.Reader, io.WriteCloser, io.WriteCloser,
		bool, <-chan remotecommand.TerminalSize) error
	ExecSyncContainer(context.Context, *Container, []string, int64) (*types.ExecSyncResponse, error)
	UpdateContainer(context.Context, *Container, *rspec.LinuxResources) error
	StopContainer(context.Context, *Container, int64) error
	DeleteContainer(context.Context, *Container) error
	UpdateContainerStatus(context.Context, *Container) error
	PauseContainer(context.Context, *Container) error
	UnpauseContainer(context.Context, *Container) error
	ContainerStats(context.Context, *Container, string) (*ContainerStats, error)
	SignalContainer(context.Context, *Container, syscall.Signal) error
	AttachContainer(context.Context, *Container, io.Reader, io.WriteCloser, io.WriteCloser,
		bool, <-chan remotecommand.TerminalSize) error
	PortForwardContainer(context.Context, *Container, string,
		int32, io.ReadWriteCloser) error
	ReopenContainerLog(context.Context, *Container) error
	WaitContainerStateStopped(context.Context, *Container) error
	CheckpointContainer(context.Context, *Container, *rspec.Spec, bool) error
	RestoreContainer(context.Context, *Container, *rspec.Spec, int, string) error
}

// New creates a new Runtime with options provided
func New(c *config.Config) *Runtime {
	return &Runtime{
		config:         c,
		runtimeImplMap: make(map[string]RuntimeImpl),
	}
}

// Runtimes returns the map of OCI runtimes.
func (r *Runtime) Runtimes() config.Runtimes {
	return r.config.Runtimes
}

// ValidateRuntimeHandler returns an error if the runtime handler string
// provided does not match any valid use case.
func (r *Runtime) ValidateRuntimeHandler(handler string) (*config.RuntimeHandler, error) {
	if handler == "" {
		return nil, fmt.Errorf("empty runtime handler")
	}

	runtimeHandler, ok := r.config.Runtimes[handler]
	if !ok {
		return nil, fmt.Errorf("failed to find runtime handler %s from runtime list %v",
			handler, r.config.Runtimes)
	}
	if runtimeHandler.RuntimePath == "" {
		return nil, fmt.Errorf("empty runtime path for runtime handler %s", handler)
	}

	return runtimeHandler, nil
}

// WaitContainerStateStopped runs a loop polling UpdateStatus(), seeking for
// the container status to be updated to 'stopped'. Either it gets the expected
// status and returns nil, or it reaches the timeout and returns an error.
func (r *Runtime) WaitContainerStateStopped(ctx context.Context, c *Container) error {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return err
	}

	// No need to go further and spawn the go routine if the container
	// is already in the expected status.
	if c.State().Status == ContainerStateStopped {
		return nil
	}

	done := make(chan error)
	chControl := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-chControl:
				return
			default:
				// Check if the container is stopped
				if err := impl.UpdateContainerStatus(ctx, c); err != nil {
					done <- err
					return
				}
				if c.State().Status == ContainerStateStopped {
					done <- nil
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()
	select {
	case err = <-done:
		break
	case <-ctx.Done():
		close(chControl)
		return ctx.Err()
	case <-time.After(time.Duration(r.config.CtrStopTimeout) * time.Second):
		close(chControl)
		return fmt.Errorf(
			"failed to get container stopped status: %ds timeout reached",
			r.config.CtrStopTimeout,
		)
	}

	if err != nil {
		return fmt.Errorf("failed to get container stopped status: %v", err)
	}

	return nil
}

func (r *Runtime) getRuntimeHandler(handler string) (*config.RuntimeHandler, error) {
	// Define the current runtime handler as the default runtime handler.
	rh := r.config.Runtimes[r.config.DefaultRuntime]

	// Override the current runtime handler with the runtime handler
	// corresponding to the runtime handler key provided with this
	// specific container.
	if handler != "" {
		runtimeHandler, err := r.ValidateRuntimeHandler(handler)
		if err != nil {
			return nil, err
		}

		rh = runtimeHandler
	}

	return rh, nil
}

// PrivelegedRuntimeHandler returns a boolean value configured for the
// runtimeHandler indicating if devices on the host are passed/not passed
// to a container running as privileged.
func (r *Runtime) PrivilegedWithoutHostDevices(handler string) (bool, error) {
	rh, err := r.getRuntimeHandler(handler)
	if err != nil {
		return false, err
	}

	return rh.PrivilegedWithoutHostDevices, nil
}

// FilterDisallowedAnnotations filters annotations that are not specified in the allowed_annotations map
// for a given handler.
// This function returns an error if the runtime handler can't be found.
// The annotations map is mutated in-place.
func (r *Runtime) FilterDisallowedAnnotations(handler string, annotations map[string]string) error {
	rh, err := r.getRuntimeHandler(handler)
	if err != nil {
		return err
	}
	for ann := range annotations {
		for _, disallowed := range rh.DisallowedAnnotations {
			if strings.HasPrefix(ann, disallowed) {
				delete(annotations, disallowed)
			}
		}
	}
	return nil
}

// RuntimeType returns the type of runtimeHandler
// This is needed when callers need to do specific work for oci vs vm
// containers, like monitor an oci container's conmon.
func (r *Runtime) RuntimeType(runtimeHandler string) (string, error) {
	rh, err := r.getRuntimeHandler(runtimeHandler)
	if err != nil {
		return "", err
	}

	return rh.RuntimeType, nil
}

func (r *Runtime) newRuntimeImpl(c *Container) (RuntimeImpl, error) {
	rh, err := r.getRuntimeHandler(c.runtimeHandler)
	if err != nil {
		return nil, err
	}

	if rh.RuntimeType == config.RuntimeTypeVM {
		return newRuntimeVM(rh.RuntimePath, rh.RuntimeRoot, rh.RuntimeConfigPath), nil
	}

	// If the runtime type is different from "vm", then let's fallback
	// onto the OCI implementation by default.
	return newRuntimeOCI(r, rh), nil
}

// RuntimeImpl returns the runtime implementation for a given container
func (r *Runtime) RuntimeImpl(c *Container) (RuntimeImpl, error) {
	r.runtimeImplMapMutex.RLock()
	impl, ok := r.runtimeImplMap[c.ID()]
	r.runtimeImplMapMutex.RUnlock()
	if !ok {
		return r.newRuntimeImpl(c)
	}

	return impl, nil
}

// CreateContainer creates a container.
func (r *Runtime) CreateContainer(ctx context.Context, c *Container, cgroupParent string, restore bool) error {
	// Instantiate a new runtime implementation for this new container
	impl, err := r.newRuntimeImpl(c)
	if err != nil {
		return err
	}

	// Assign this runtime implementation to the current container
	r.runtimeImplMapMutex.Lock()
	r.runtimeImplMap[c.ID()] = impl
	r.runtimeImplMapMutex.Unlock()

	return impl.CreateContainer(ctx, c, cgroupParent, restore)
}

// StartContainer starts a container.
func (r *Runtime) StartContainer(ctx context.Context, c *Container) error {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return err
	}

	return impl.StartContainer(ctx, c)
}

// ExecContainer prepares a streaming endpoint to execute a command in the container.
func (r *Runtime) ExecContainer(ctx context.Context, c *Container, cmd []string, stdin io.Reader, stdout, stderr io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return err
	}

	return impl.ExecContainer(ctx, c, cmd, stdin, stdout, stderr, tty, resize)
}

// ExecSyncContainer execs a command in a container and returns it's stdout, stderr and return code.
func (r *Runtime) ExecSyncContainer(ctx context.Context, c *Container, command []string, timeout int64) (*types.ExecSyncResponse, error) {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return nil, err
	}

	return impl.ExecSyncContainer(ctx, c, command, timeout)
}

// UpdateContainer updates container resources
func (r *Runtime) UpdateContainer(ctx context.Context, c *Container, res *rspec.LinuxResources) error {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return err
	}

	return impl.UpdateContainer(ctx, c, res)
}

// StopContainer stops a container. Timeout is given in seconds.
func (r *Runtime) StopContainer(ctx context.Context, c *Container, timeout int64) error {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return err
	}

	return impl.StopContainer(ctx, c, timeout)
}

// DeleteContainer deletes a container.
func (r *Runtime) DeleteContainer(ctx context.Context, c *Container) (err error) {
	r.runtimeImplMapMutex.RLock()
	impl, ok := r.runtimeImplMap[c.ID()]
	r.runtimeImplMapMutex.RUnlock()
	if !ok {
		if impl, err = r.newRuntimeImpl(c); err != nil {
			return err
		}
	} else {
		defer func() {
			if err == nil {
				r.runtimeImplMapMutex.Lock()
				delete(r.runtimeImplMap, c.ID())
				r.runtimeImplMapMutex.Unlock()
			}
		}()
	}

	return impl.DeleteContainer(ctx, c)
}

// UpdateContainerStatus refreshes the status of the container.
func (r *Runtime) UpdateContainerStatus(ctx context.Context, c *Container) error {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return err
	}

	return impl.UpdateContainerStatus(ctx, c)
}

// PauseContainer pauses a container.
func (r *Runtime) PauseContainer(ctx context.Context, c *Container) error {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return err
	}

	return impl.PauseContainer(ctx, c)
}

// UnpauseContainer unpauses a container.
func (r *Runtime) UnpauseContainer(ctx context.Context, c *Container) error {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return err
	}

	return impl.UnpauseContainer(ctx, c)
}

// ContainerStats provides statistics of a container.
func (r *Runtime) ContainerStats(ctx context.Context, c *Container, cgroup string) (*ContainerStats, error) {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return nil, err
	}

	return impl.ContainerStats(ctx, c, cgroup)
}

// SignalContainer sends a signal to a container process.
func (r *Runtime) SignalContainer(ctx context.Context, c *Container, sig syscall.Signal) error {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return err
	}

	return impl.SignalContainer(ctx, c, sig)
}

// AttachContainer attaches IO to a running container.
func (r *Runtime) AttachContainer(ctx context.Context, c *Container, inputStream io.Reader, outputStream, errorStream io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return err
	}

	return impl.AttachContainer(ctx, c, inputStream, outputStream, errorStream, tty, resize)
}

// PortForwardContainer forwards the specified port provides statistics of a container.
func (r *Runtime) PortForwardContainer(ctx context.Context, c *Container, netNsPath string, port int32, stream io.ReadWriteCloser) error {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return err
	}

	return impl.PortForwardContainer(ctx, c, netNsPath, port, stream)
}

// ReopenContainerLog reopens the log file of a container.
func (r *Runtime) ReopenContainerLog(ctx context.Context, c *Container) error {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return err
	}

	return impl.ReopenContainerLog(ctx, c)
}

// BuildContainerdBinaryName() is responsible for ensuring the binary passed will
// be properly converted to the containerd binary naming pattern.
//
// This method should never ever be called from anywhere else but the runtimeVM,
// and the only reason its exported here is in order to get some test coverage.
func BuildContainerdBinaryName(path string) string {
	// containerd expects the runtime name to be in the following pattern:
	//        ($dir.)?$prefix.$name.$version
	//        -------- ------ ----- ---------
	//              |     |     |      |
	//              v     |     |      |
	//      "/usr/local/bin"    |      |
	//        (optional)  |     |      |
	//                    v     |      |
	//             "containerd.shim."  |
	//                          |      |
	//                          v      |
	//                     "kata-qemu" |
	//                                 v
	//                                "v2"
	const expectedPrefix = "containerd-shim-"
	const expectedVersion = "-v2"

	const binaryPrefix = "containerd.shim"
	const binaryVersion = "v2"

	runtimeDir := filepath.Dir(path)
	// This is only safe to do because the runtime_path, for the VM runtime_type, is validated in the config,
	// allowing us to take the liberty to simply go ahead and check, without having to ensure we're receiving
	// the binary in the expected form.
	//
	// For clarity, it could be ensured twice, but we count on the developer to never ever call this function
	// in a different context from the one used in the runtime_vm.go file.
	runtimeName := strings.SplitAfter(strings.Split(filepath.Base(path), expectedVersion)[0], expectedPrefix)[1]

	return filepath.Join(runtimeDir, fmt.Sprintf("%s.%s.%s", binaryPrefix, runtimeName, binaryVersion))
}

// CheckpointContainer checkpoints a container.
func (r *Runtime) CheckpointContainer(ctx context.Context, c *Container, specgen *rspec.Spec, leaveRunning bool) error {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return err
	}

	return impl.CheckpointContainer(ctx, c, specgen, leaveRunning)
}

// RestoreContainer restores a container.
func (r *Runtime) RestoreContainer(ctx context.Context, c *Container, sbSpec *rspec.Spec, infraPid int, cgroupParent string) error {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return err
	}

	return impl.RestoreContainer(ctx, c, sbSpec, infraPid, cgroupParent)
}
