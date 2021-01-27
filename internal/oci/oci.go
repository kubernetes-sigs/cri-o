package oci

import (
	"bytes"
	"fmt"
	"io"
	"sync"
	"syscall"
	"time"

	"github.com/cri-o/cri-o/pkg/annotations"
	"github.com/cri-o/cri-o/pkg/config"
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
	CreateContainer(*Container, string) error
	StartContainer(*Container) error
	ExecContainer(*Container, []string, io.Reader, io.WriteCloser, io.WriteCloser,
		bool, <-chan remotecommand.TerminalSize) error
	ExecSyncContainer(*Container, []string, int64) (*ExecSyncResponse, error)
	UpdateContainer(*Container, *rspec.LinuxResources) error
	StopContainer(context.Context, *Container, int64) error
	DeleteContainer(*Container) error
	UpdateContainerStatus(*Container) error
	PauseContainer(*Container) error
	UnpauseContainer(*Container) error
	ContainerStats(*Container, string) (*ContainerStats, error)
	SignalContainer(*Container, syscall.Signal) error
	AttachContainer(*Container, io.Reader, io.WriteCloser, io.WriteCloser,
		bool, <-chan remotecommand.TerminalSize) error
	PortForwardContainer(context.Context, *Container, string,
		int32, io.ReadWriteCloser) error
	ReopenContainerLog(*Container) error
	WaitContainerStateStopped(context.Context, *Container) error
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
				if err := impl.UpdateContainerStatus(c); err != nil {
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

	// add the runtime config allowed annotations to the runtime handler allowed annotations
	rh.AllowedAnnotations = append(r.config.AllowedAnnotations, rh.AllowedAnnotations...)

	// remove duplicates from the allowed annotations
	keys := make(map[string]bool)
	list := []string{}

	for _, entry := range rh.AllowedAnnotations {
		if _, value := keys[entry]; !value {
			keys[entry] = true
			list = append(list, entry)
		}
	}
	rh.AllowedAnnotations = list

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

// AllowUsernsAnnotation searches through the AllowedAnnotations for
// the userns annotation, checking whether this runtime allows processing of "io.kubernetes.cri-o.userns-mode"
func (r *Runtime) AllowUsernsAnnotation(handler string) (bool, error) {
	return r.allowAnnotation(handler, annotations.UsernsModeAnnotation)
}

// AllowDevicesAnnotation searches through the AllowedAnnotations for
// the devices annotation, checking whether this runtime allows processing of "io.kubernetes.cri-o.Devices"
func (r *Runtime) AllowDevicesAnnotation(handler string) (bool, error) {
	return r.allowAnnotation(handler, annotations.DevicesAnnotation)
}

// AllowCPULoadBalancingAnnotation searches through the AllowedAnnotations for
// the CPU load balancing annotation, checking whether this runtime allows processing of  "cpu-load-balancing.crio.io"
func (r *Runtime) AllowCPULoadBalancingAnnotation(handler string) (bool, error) {
	return r.allowAnnotation(handler, annotations.CPULoadBalancingAnnotation)
}

// AllowCPUQuotaAnnotation searches through the AllowedAnnotations for
// the CPU quota annotation, checking whether this runtime allows processing of "cpu-quota.crio.io"
func (r *Runtime) AllowCPUQuotaAnnotation(handler string) (bool, error) {
	return r.allowAnnotation(handler, annotations.CPUQuotaAnnotation)
}

// AllowIRQLoadBalancingAnnotation searches through the AllowedAnnotations for
// the IRQ load balancing annotation, checking whether this runtime allows processing of "irq-load-balancing.crio.io"
func (r *Runtime) AllowIRQLoadBalancingAnnotation(handler string) (bool, error) {
	return r.allowAnnotation(handler, annotations.IRQLoadBalancingAnnotation)
}

func (r *Runtime) AllowShmSizeAnnotation(handler string) (bool, error) {
	return r.allowAnnotation(handler, annotations.ShmSizeAnnotation)
}

func (r *Runtime) allowAnnotation(handler, annotation string) (bool, error) {
	rh, err := r.getRuntimeHandler(handler)
	if err != nil {
		return false, err
	}
	for _, ann := range rh.AllowedAnnotations {
		if ann == annotation {
			return true, nil
		}
	}

	return false, nil
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
		return newRuntimeVM(rh.RuntimePath), nil
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
func (r *Runtime) CreateContainer(c *Container, cgroupParent string) error {
	// Instantiate a new runtime implementation for this new container
	impl, err := r.newRuntimeImpl(c)
	if err != nil {
		return err
	}

	// Assign this runtime implementation to the current container
	r.runtimeImplMapMutex.Lock()
	r.runtimeImplMap[c.ID()] = impl
	r.runtimeImplMapMutex.Unlock()

	return impl.CreateContainer(c, cgroupParent)
}

// StartContainer starts a container.
func (r *Runtime) StartContainer(c *Container) error {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return err
	}

	return impl.StartContainer(c)
}

// ExecContainer prepares a streaming endpoint to execute a command in the container.
func (r *Runtime) ExecContainer(c *Container, cmd []string, stdin io.Reader, stdout, stderr io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return err
	}

	return impl.ExecContainer(c, cmd, stdin, stdout, stderr, tty, resize)
}

// ExecSyncContainer execs a command in a container and returns it's stdout, stderr and return code.
func (r *Runtime) ExecSyncContainer(c *Container, command []string, timeout int64) (*ExecSyncResponse, error) {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return nil, err
	}

	return impl.ExecSyncContainer(c, command, timeout)
}

// UpdateContainer updates container resources
func (r *Runtime) UpdateContainer(c *Container, res *rspec.LinuxResources) error {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return err
	}

	return impl.UpdateContainer(c, res)
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
func (r *Runtime) DeleteContainer(c *Container) (err error) {
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

	return impl.DeleteContainer(c)
}

// UpdateContainerStatus refreshes the status of the container.
func (r *Runtime) UpdateContainerStatus(c *Container) error {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return err
	}

	return impl.UpdateContainerStatus(c)
}

// PauseContainer pauses a container.
func (r *Runtime) PauseContainer(c *Container) error {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return err
	}

	return impl.PauseContainer(c)
}

// UnpauseContainer unpauses a container.
func (r *Runtime) UnpauseContainer(c *Container) error {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return err
	}

	return impl.UnpauseContainer(c)
}

// ContainerStats provides statistics of a container.
func (r *Runtime) ContainerStats(c *Container, cgroup string) (*ContainerStats, error) {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return nil, err
	}

	return impl.ContainerStats(c, cgroup)
}

// SignalContainer sends a signal to a container process.
func (r *Runtime) SignalContainer(c *Container, sig syscall.Signal) error {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return err
	}

	return impl.SignalContainer(c, sig)
}

// AttachContainer attaches IO to a running container.
func (r *Runtime) AttachContainer(c *Container, inputStream io.Reader, outputStream, errorStream io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return err
	}

	return impl.AttachContainer(c, inputStream, outputStream, errorStream, tty, resize)
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
func (r *Runtime) ReopenContainerLog(c *Container) error {
	impl, err := r.RuntimeImpl(c)
	if err != nil {
		return err
	}

	return impl.ReopenContainerLog(c)
}

// ExecSyncResponse is returned from ExecSync.
type ExecSyncResponse struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int32
}

// ExecSyncError wraps command's streams, exit code and error on ExecSync error.
type ExecSyncError struct {
	Stdout   bytes.Buffer
	Stderr   bytes.Buffer
	ExitCode int32
	Err      error
}

func (e *ExecSyncError) Error() string {
	return fmt.Sprintf("command error: %+v, stdout: %s, stderr: %s, exit code %d", e.Err, e.Stdout.Bytes(), e.Stderr.Bytes(), e.ExitCode)
}
