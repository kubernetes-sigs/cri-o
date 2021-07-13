package oci

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	cgroups "github.com/containerd/cgroups/stats/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/namespaces"
	client "github.com/containerd/containerd/runtime/v2/shim"
	"github.com/containerd/containerd/runtime/v2/task"
	runtimeoptions "github.com/containerd/cri-containerd/pkg/api/runtimeoptions/v1"
	"github.com/containerd/ttrpc"
	"github.com/containerd/typeurl"
	conmonconfig "github.com/containers/conmon/runner/config"
	"github.com/cri-o/cri-o/internal/log"
	"github.com/cri-o/cri-o/server/cri/types"
	"github.com/cri-o/cri-o/server/metrics"
	"github.com/cri-o/cri-o/utils"
	"github.com/cri-o/cri-o/utils/errdefs"
	"github.com/cri-o/cri-o/utils/fifo"
	cio "github.com/cri-o/cri-o/utils/io"
	cioutil "github.com/cri-o/cri-o/utils/ioutil"
	ptypes "github.com/gogo/protobuf/types"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
	"k8s.io/client-go/tools/remotecommand"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	utilexec "k8s.io/utils/exec"
)

// runtimeVM is the Runtime interface implementation that is more appropriate
// for VM based container runtimes.
type runtimeVM struct {
	path       string
	fifoDir    string
	configPath string
	ctx        context.Context
	client     *ttrpc.Client
	task       task.TaskService

	sync.Mutex
	ctrs map[string]containerInfo
}

type containerInfo struct {
	cio *cio.ContainerIO
}

const (
	execError   = -1
	execTimeout = -2
)

// newRuntimeVM creates a new runtimeVM instance
func newRuntimeVM(path, root, configPath string) RuntimeImpl {
	logrus.Debug("oci.newRuntimeVM() start")
	defer logrus.Debug("oci.newRuntimeVM() end")

	// FIXME: We need to register those types for now, but this should be
	// defined as a specific package that would be shared both by CRI-O and
	// containerd. This would allow shim implementation to import a single
	// package to do the proper registration.
	const prefix = "types.containerd.io"
	// register TypeUrls for commonly marshaled external types
	major := strconv.Itoa(rspec.VersionMajor)
	typeurl.Register(&rspec.Spec{}, prefix, "opencontainers/runtime-spec", major, "Spec")
	typeurl.Register(&rspec.Process{}, prefix, "opencontainers/runtime-spec", major, "Process")
	typeurl.Register(&rspec.LinuxResources{}, prefix, "opencontainers/runtime-spec", major, "LinuxResources")
	typeurl.Register(&rspec.WindowsResources{}, prefix, "opencontainers/runtime-spec", major, "WindowsResources")

	return &runtimeVM{
		path:       path,
		configPath: configPath,
		fifoDir:    filepath.Join(root, "crio", "fifo"),
		ctx:        context.Background(),
		ctrs:       make(map[string]containerInfo),
	}
}

// CreateContainer creates a container.
func (r *runtimeVM) CreateContainer(ctx context.Context, c *Container, cgroupParent string, restore bool) (retErr error) {
	log.Debugf(ctx, "RuntimeVM.CreateContainer() start")
	defer log.Debugf(ctx, "RuntimeVM.CreateContainer() end")

	// Lock the container
	c.opLock.Lock()
	defer c.opLock.Unlock()

	// Lets ensure we're able to properly get construct the Options
	// that we'll pass to the ContainerCreateTask, as admins can set
	// the runtime_config_path to an arbitrary location.  Also, lets
	// fail early if something goes wrong.
	var opts *ptypes.Any = nil
	if r.configPath != "" {
		runtimeOptions := &runtimeoptions.Options{
			ConfigPath: r.configPath,
		}

		marshaledOtps, err := typeurl.MarshalAny(runtimeOptions)
		if err != nil {
			return err
		}

		opts = marshaledOtps
	}

	// First thing, we need to start the runtime daemon
	if err := r.startRuntimeDaemon(ctx, c); err != nil {
		return err
	}

	// Create IO fifos
	containerIO, err := cio.NewContainerIO(c.ID(),
		cio.WithNewFIFOs(r.fifoDir, c.terminal, c.stdin))
	if err != nil {
		return err
	}

	defer func() {
		if retErr != nil {
			containerIO.Close()
		}
	}()

	f, err := os.OpenFile(c.LogPath(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}

	var stdoutCh, stderrCh <-chan struct{}
	wc := cioutil.NewSerialWriteCloser(f)
	stdout, stdoutCh := cio.NewCRILogger(c.LogPath(), wc, cio.Stdout, -1)
	stderr, stderrCh := cio.NewCRILogger(c.LogPath(), wc, cio.Stderr, -1)

	go func() {
		if stdoutCh != nil {
			<-stdoutCh
		}
		if stderrCh != nil {
			<-stderrCh
		}
		log.Debugf(ctx, "Finish redirecting log file %q, closing it", c.LogPath())
		f.Close()
	}()

	containerIO.AddOutput(c.LogPath(), stdout, stderr)
	containerIO.Pipe()

	r.Lock()
	r.ctrs[c.ID()] = containerInfo{
		cio: containerIO,
	}
	r.Unlock()

	defer func() {
		if retErr != nil {
			log.Warnf(ctx, "Cleaning up container %s: %v", c.ID(), err)
			if cleanupErr := r.deleteContainer(c, true); cleanupErr != nil {
				log.Infof(ctx, "DeleteContainer failed for container %s: %v", c.ID(), cleanupErr)
			}
		}
	}()

	// We can now create the container, interacting with the server
	request := &task.CreateTaskRequest{
		ID:       c.ID(),
		Bundle:   c.BundlePath(),
		Stdin:    containerIO.Config().Stdin,
		Stdout:   containerIO.Config().Stdout,
		Stderr:   containerIO.Config().Stderr,
		Terminal: containerIO.Config().Terminal,
		Options:  opts,
	}

	createdCh := make(chan error)
	go func() {
		// Create the container
		if resp, err := r.task.Create(r.ctx, request); err != nil {
			createdCh <- errdefs.FromGRPC(err)
		} else if err := c.state.SetInitPid(int(resp.Pid)); err != nil {
			createdCh <- err
		}

		close(createdCh)
	}()

	select {
	case err = <-createdCh:
		if err != nil {
			return errors.Errorf("CreateContainer failed: %v", err)
		}
	case <-time.After(ContainerCreateTimeout):
		if err := r.remove(c.ID(), ""); err != nil {
			return err
		}
		<-createdCh
		return errors.Errorf("CreateContainer timeout (%v)", ContainerCreateTimeout)
	}

	return nil
}

func (r *runtimeVM) startRuntimeDaemon(ctx context.Context, c *Container) error {
	log.Debugf(ctx, "RuntimeVM.startRuntimeDaemon() start")
	defer log.Debugf(ctx, "RuntimeVM.startRuntimeDaemon() end")

	// Prepare the command to run
	args := []string{"-id", c.ID()}
	switch logrus.GetLevel() {
	case logrus.DebugLevel, logrus.TraceLevel:
		args = append(args, "-debug")
	}
	args = append(args, "start")

	// Modify the runtime path so that it complies with v2 shim API
	newRuntimePath := BuildContainerdBinaryName(r.path)

	r.ctx = namespaces.WithNamespace(r.ctx, namespaces.Default)

	// Prepare the command to exec
	cmd, err := client.Command(
		r.ctx,
		newRuntimePath,
		"",
		"",
		c.BundlePath(),
		nil,
		args...,
	)
	if err != nil {
		return err
	}

	// Create the log file expected by shim-v2 API
	f, err := fifo.OpenFifo(r.ctx, filepath.Join(c.BundlePath(), "log"),
		unix.O_RDONLY|unix.O_CREAT|unix.O_NONBLOCK, 0o700)
	if err != nil {
		return err
	}

	// Open the log pipe and block until the writer is ready. This
	// helps with synchronization of the shim. Copy the shim's logs
	// to CRI-O's output.
	go func() {
		defer f.Close()
		if _, err := io.Copy(os.Stderr, f); err != nil {
			log.Errorf(ctx, "Copy shim log: %v", err)
		}
	}()

	// Start the server
	out, err := cmd.CombinedOutput()
	if err != nil {
		return errors.Wrap(err, string(out))
	}

	// Retrieve the address from the output
	address := strings.TrimSpace(string(out))

	// Now the RPC server is running, let's connect to it
	conn, err := client.Connect(address, client.AnonDialer)
	if err != nil {
		return err
	}

	options := ttrpc.WithOnClose(func() { conn.Close() })
	cl := ttrpc.NewClient(conn, options)

	// Update the runtime structure
	r.client = cl
	r.task = task.NewTaskClient(cl)

	return nil
}

// StartContainer starts a container.
func (r *runtimeVM) StartContainer(ctx context.Context, c *Container) error {
	log.Debugf(ctx, "RuntimeVM.StartContainer() start")
	defer log.Debugf(ctx, "RuntimeVM.StartContainer() end")

	// Lock the container
	c.opLock.Lock()
	defer c.opLock.Unlock()

	if err := r.start(c.ID(), ""); err != nil {
		return err
	}
	c.state.Started = time.Now()

	// Spawn a goroutine waiting for the container to terminate. Once it
	// happens, the container status is retrieved to be updated.
	go func() {
		_, err := r.wait(c.ID(), "")
		if err == nil {
			if err1 := r.updateContainerStatus(ctx, c); err1 != nil {
				log.Warnf(ctx, "Error updating container status %v", err1)
			}
		} else {
			log.Warnf(ctx, "Wait for %s returned: %v", c.ID(), err)
		}
	}()

	return nil
}

// ExecContainer prepares a streaming endpoint to execute a command in the container.
func (r *runtimeVM) ExecContainer(ctx context.Context, c *Container, cmd []string, stdin io.Reader, stdout, stderr io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	log.Debugf(ctx, "RuntimeVM.ExecContainer() start")
	defer log.Debugf(ctx, "RuntimeVM.ExecContainer() end")

	exitCode, err := r.execContainerCommon(ctx, c, cmd, 0, stdin, stdout, stderr, tty, resize)
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return &utilexec.CodeExitError{
			Err:  errors.Errorf("error executing command %v, exit code %d", cmd, exitCode),
			Code: int(exitCode),
		}
	}

	return nil
}

// ExecSyncContainer execs a command in a container and returns it's stdout, stderr and return code.
func (r *runtimeVM) ExecSyncContainer(ctx context.Context, c *Container, command []string, timeout int64) (*types.ExecSyncResponse, error) {
	log.Debugf(ctx, "RuntimeVM.ExecSyncContainer() start")
	defer log.Debugf(ctx, "RuntimeVM.ExecSyncContainer() end")

	var stdoutBuf, stderrBuf bytes.Buffer
	stdout := cioutil.NewNopWriteCloser(&stdoutBuf)
	stderr := cioutil.NewNopWriteCloser(&stderrBuf)

	exitCode, err := r.execContainerCommon(ctx, c, command, timeout, nil, stdout, stderr, c.terminal, nil)
	if err != nil {
		return nil, errors.Wrap(err, "ExecSyncContainer failed")
	}

	// if the execution stopped because of the timeout, report it as such
	if exitCode == execTimeout {
		return &types.ExecSyncResponse{
			Stderr:   []byte(conmonconfig.TimedOutMessage),
			ExitCode: -1,
		}, nil
	}

	return &types.ExecSyncResponse{
		Stdout:   stdoutBuf.Bytes(),
		Stderr:   stderrBuf.Bytes(),
		ExitCode: exitCode,
	}, nil
}

func (r *runtimeVM) execContainerCommon(ctx context.Context, c *Container, cmd []string, timeout int64, stdin io.Reader, stdout, stderr io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) (exitCode int32, retErr error) {
	log.Debugf(ctx, "RuntimeVM.execContainerCommon() start")
	defer log.Debugf(ctx, "RuntimeVM.execContainerCommon() end")

	// Cancel the context before returning to ensure goroutines are stopped.
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()

	// Generate a unique execID
	execID, err := utils.GenerateID()
	if err != nil {
		return execError, errors.Wrap(err, "exec container")
	}

	// Create IO fifos
	execIO, err := cio.NewExecIO(c.ID(), r.fifoDir, tty, stdin != nil)
	if err != nil {
		return execError, errdefs.FromGRPC(err)
	}
	defer execIO.Close()

	// chan to notify that can call runtime's CloseIO API
	closeIOChan := make(chan bool)
	defer func() {
		if closeIOChan != nil {
			close(closeIOChan)
		}
	}()

	execIO.Attach(cio.AttachOptions{
		Stdin:     stdin,
		Stdout:    stdout,
		Stderr:    stderr,
		Tty:       tty,
		StdinOnce: true,
		CloseStdin: func() error {
			if closeIOChan != nil {
				<-closeIOChan
			}
			return r.closeIO(c.ID(), execID)
		},
	})

	pSpec := c.Spec().Process
	pSpec.Args = cmd

	any, err := typeurl.MarshalAny(pSpec)
	if err != nil {
		return execError, errdefs.FromGRPC(err)
	}

	request := &task.ExecProcessRequest{
		ID:       c.ID(),
		ExecID:   execID,
		Stdin:    execIO.Config().Stdin,
		Stdout:   execIO.Config().Stdout,
		Stderr:   execIO.Config().Stderr,
		Terminal: execIO.Config().Terminal,
		Spec:     any,
	}

	// Create the "exec" process
	if _, err = r.task.Exec(r.ctx, request); err != nil {
		return execError, errdefs.FromGRPC(err)
	}

	defer func() {
		if retErr != nil {
			if err := r.remove(c.ID(), execID); err != nil {
				log.Debugf(ctx, "Unable to remove container %s: %v", c.ID(), err)
			}
		}
	}()

	// Start the process
	if err := r.start(c.ID(), execID); err != nil {
		return execError, err
	}

	// close closeIOChan to notify execIO exec has started.
	close(closeIOChan)
	closeIOChan = nil

	// Initialize terminal resizing if necessary
	if resize != nil {
		kubecontainer.HandleResizing(resize, func(size remotecommand.TerminalSize) {
			log.Debugf(ctx, "Got a resize event: %+v", size)

			if err := r.resizePty(c.ID(), execID, size); err != nil {
				log.Warnf(ctx, "Failed to resize terminal: %v", err)
			}
		})
	}

	timeoutDuration := time.Duration(timeout) * time.Second

	var timeoutCh <-chan time.Time
	if timeoutDuration == 0 {
		// Do not set timeout if it's 0
		timeoutCh = make(chan time.Time)
	} else {
		timeoutCh = time.After(timeoutDuration)
	}

	execCh := make(chan error)
	go func() {
		// Wait for the process to terminate
		exitCode, err = r.wait(c.ID(), execID)
		if err != nil {
			execCh <- err
		}

		close(execCh)
	}()

	select {
	case err = <-execCh:
		if err != nil {
			if killErr := r.kill(c.ID(), execID, syscall.SIGKILL, false); killErr != nil {
				return execError, killErr
			}
			return execError, err
		}
	case <-timeoutCh:
		if killErr := r.kill(c.ID(), execID, syscall.SIGKILL, false); killErr != nil {
			return execError, killErr
		}
		<-execCh
		// do not make an error for timeout: report it with a specific error code
		return execTimeout, nil
	}

	if err == nil {
		// Delete the process
		if err := r.remove(c.ID(), execID); err != nil {
			log.Debugf(ctx, "Unable to remove container %s: %v", c.ID(), err)
		}
	}

	return exitCode, err
}

// UpdateContainer updates container resources
func (r *runtimeVM) UpdateContainer(ctx context.Context, c *Container, res *rspec.LinuxResources) error {
	log.Debugf(ctx, "RuntimeVM.UpdateContainer() start")
	defer log.Debugf(ctx, "RuntimeVM.UpdateContainer() end")

	// Lock the container
	c.opLock.Lock()
	defer c.opLock.Unlock()

	// Convert resources into protobuf Any type
	any, err := typeurl.MarshalAny(res)
	if err != nil {
		return err
	}

	if _, err := r.task.Update(r.ctx, &task.UpdateTaskRequest{
		ID:        c.ID(),
		Resources: any,
	}); err != nil {
		return errdefs.FromGRPC(err)
	}

	return nil
}

// StopContainer stops a container. Timeout is given in seconds.
func (r *runtimeVM) StopContainer(ctx context.Context, c *Container, timeout int64) error {
	log.Debugf(ctx, "RuntimeVM.StopContainer() start")
	defer log.Debugf(ctx, "RuntimeVM.StopContainer() end")

	// Lock the container
	c.opLock.Lock()
	defer c.opLock.Unlock()

	if err := c.ShouldBeStopped(); err != nil {
		return err
	}

	// Cancel the context before returning to ensure goroutines are stopped.
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()

	stopCh := make(chan error)
	go func() {
		// errdefs.ErrNotFound actually comes from a closed connection, which is expected
		// when stoping the container, with the agent and the VM going off. In such case.
		// let's just ignore the error.
		if _, err := r.wait(c.ID(), ""); err != nil && !errors.Is(err, errdefs.ErrNotFound) {
			stopCh <- errdefs.FromGRPC(err)
		}

		close(stopCh)
	}()

	var sig syscall.Signal

	if timeout > 0 {
		sig = c.StopSignal()
		// Send a stopping signal to the container
		if err := r.kill(c.ID(), "", sig, false); err != nil {
			return err
		}

		timeoutDuration := time.Duration(timeout) * time.Second

		err := r.waitCtrTerminate(sig, stopCh, timeoutDuration)
		if err == nil {
			c.state.Finished = time.Now()
			return nil
		}
		log.Warnf(ctx, "%v", err)
	}

	sig = syscall.SIGKILL
	// Send a SIGKILL signal to the container
	if err := r.kill(c.ID(), "", sig, false); err != nil {
		return err
	}

	if err := r.waitCtrTerminate(sig, stopCh, killContainerTimeout); err != nil {
		log.Errorf(ctx, "%v", err)
		return err
	}

	c.state.Finished = time.Now()
	return nil
}

func (r *runtimeVM) waitCtrTerminate(sig syscall.Signal, stopCh chan error, timeout time.Duration) error {
	select {
	case err := <-stopCh:
		return err
	case <-time.After(timeout):
		return errors.Errorf("StopContainer with signal %v timed out after (%v)", sig, timeout)
	}
}

// DeleteContainer deletes a container.
func (r *runtimeVM) DeleteContainer(ctx context.Context, c *Container) error {
	log.Debugf(ctx, "RuntimeVM.DeleteContainer() start")
	defer log.Debugf(ctx, "RuntimeVM.DeleteContainer() end")

	// Lock the container
	c.opLock.Lock()
	defer c.opLock.Unlock()

	return r.deleteContainer(c, false)
}

// deleteContainer performs all the operations needed to delete a container.
// force must only be used on clean-up cases.
// It does **not** Lock the container, thus it's the caller responsibility to do so, when needed.
func (r *runtimeVM) deleteContainer(c *Container, force bool) error {
	r.Lock()
	cInfo, ok := r.ctrs[c.ID()]
	r.Unlock()
	if !ok && !force {
		return errors.New("Could not retrieve container information")
	}

	if err := cInfo.cio.Close(); err != nil && !force {
		return err
	}

	if err := r.remove(c.ID(), ""); err != nil && !force {
		return err
	}

	_, err := r.task.Shutdown(r.ctx, &task.ShutdownRequest{ID: c.ID()})
	if err != nil && !errors.Is(err, ttrpc.ErrClosed) && !force {
		return err
	}

	r.Lock()
	delete(r.ctrs, c.ID())
	r.Unlock()

	return nil
}

// UpdateContainerStatus refreshes the status of the container.
func (r *runtimeVM) UpdateContainerStatus(ctx context.Context, c *Container) error {
	log.Debugf(ctx, "RuntimeVM.UpdateContainerStatus() start")
	defer log.Debugf(ctx, "RuntimeVM.UpdateContainerStatus() end")

	// Lock the container
	c.opLock.Lock()
	defer c.opLock.Unlock()

	return r.updateContainerStatus(ctx, c)
}

// updateContainerStatus is a UpdateContainerStatus helper, which actually does the container's
// status refresh.
// It does **not** Lock the container, thus it's the caller responsibility to do so, when needed.
func (r *runtimeVM) updateContainerStatus(ctx context.Context, c *Container) error {
	log.Debugf(ctx, "RuntimeVM.updateContainerStatus() start")
	defer log.Debugf(ctx, "RuntimeVM.updateContainerStatus() end")

	// This can happen on restore, for example if we switch the runtime type
	// for a container from "oci" to "vm" for the same runtime.
	if r.task == nil {
		return errors.New("runtime not correctly setup")
	}

	response, err := r.task.State(r.ctx, &task.StateRequest{
		ID: c.ID(),
	})
	if err != nil {
		if !errors.Is(err, ttrpc.ErrClosed) {
			return errdefs.FromGRPC(err)
		}
		return errdefs.ErrNotFound
	}

	status := c.state.Status
	switch response.Status {
	case tasktypes.StatusCreated:
		status = ContainerStateCreated
	case tasktypes.StatusRunning:
		status = ContainerStateRunning
	case tasktypes.StatusStopped:
		status = ContainerStateStopped
	case tasktypes.StatusPaused:
		status = ContainerStatePaused
	}

	c.state.Status = status
	c.state.Finished = response.ExitedAt
	exitCode := int32(response.ExitStatus)
	c.state.ExitCode = &exitCode
	c.state.Pid = int(response.Pid)

	if exitCode != 0 {
		oomFilePath := filepath.Join(c.bundlePath, "oom")
		if _, err = os.Stat(oomFilePath); err == nil {
			c.state.OOMKilled = true

			// Collect total metric
			metrics.Instance().MetricContainersOOMTotalInc()

			// Collect metric by container name
			metrics.Instance().MetricContainersOOMInc(c.Name())
		}
	}
	return nil
}

// PauseContainer pauses a container.
func (r *runtimeVM) PauseContainer(ctx context.Context, c *Container) error {
	log.Debugf(ctx, "RuntimeVM.PauseContainer() start")
	defer log.Debugf(ctx, "RuntimeVM.PauseContainer() end")

	// Lock the container
	c.opLock.Lock()
	defer c.opLock.Unlock()

	if _, err := r.task.Pause(r.ctx, &task.PauseRequest{
		ID: c.ID(),
	}); err != nil {
		return errdefs.FromGRPC(err)
	}

	return nil
}

// UnpauseContainer unpauses a container.
func (r *runtimeVM) UnpauseContainer(ctx context.Context, c *Container) error {
	log.Debugf(ctx, "RuntimeVM.UnpauseContainer() start")
	defer log.Debugf(ctx, "RuntimeVM.UnpauseContainer() end")

	// Lock the container
	c.opLock.Lock()
	defer c.opLock.Unlock()

	if _, err := r.task.Resume(r.ctx, &task.ResumeRequest{
		ID: c.ID(),
	}); err != nil {
		return errdefs.FromGRPC(err)
	}

	return nil
}

// ContainerStats provides statistics of a container.
func (r *runtimeVM) ContainerStats(ctx context.Context, c *Container, _ string) (*ContainerStats, error) {
	log.Debugf(ctx, "RuntimeVM.ContainerStats() start")
	defer log.Debugf(ctx, "RuntimeVM.ContainerStats() end")

	// Lock the container with a shared lock
	c.opLock.RLock()
	defer c.opLock.RUnlock()

	resp, err := r.task.Stats(r.ctx, &task.StatsRequest{
		ID: c.ID(),
	})
	if err != nil {
		return nil, errdefs.FromGRPC(err)
	}
	if resp == nil {
		return nil, errors.New("Could not retrieve container stats")
	}

	stats, err := typeurl.UnmarshalAny(resp.Stats)
	if err != nil {
		return nil, errors.Wrap(err, "failed to extract container metrics")
	}

	m, ok := stats.(*cgroups.Metrics)
	if !ok {
		return nil, errors.Errorf("Unknown stats type %T", stats)
	}

	return metricsToCtrStats(ctx, c, m), nil
}

func metricsToCtrStats(ctx context.Context, c *Container, m *cgroups.Metrics) *ContainerStats {
	var (
		blockInput      uint64
		blockOutput     uint64
		cpu             float64
		cpuNano         uint64
		memLimit        uint64
		memPerc         float64
		memUsage        uint64
		netInput        uint64
		netOutput       uint64
		pids            uint64
		workingSetBytes uint64
	)

	if m != nil {
		pids = m.Pids.Current

		cpuNano = m.CPU.Usage.Total
		cpu = genericCalculateCPUPercent(cpuNano, m.CPU.Usage.PerCPU)

		memUsage = m.Memory.Usage.Usage
		memLimit = getMemLimit(m.Memory.Usage.Limit)
		memPerc = float64(memUsage) / float64(memLimit)
		if memUsage > m.Memory.TotalInactiveFile {
			workingSetBytes = memUsage - m.Memory.TotalInactiveFile
		} else {
			log.Debugf(ctx,
				"Unable to account working set stats: total_inactive_file (%d) > memory usage (%d)",
				m.Memory.TotalInactiveFile, memUsage,
			)
		}

		for _, entry := range m.Blkio.IoServiceBytesRecursive {
			switch strings.ToLower(entry.Op) {
			case "read":
				blockInput += entry.Value
			case "write":
				blockOutput += entry.Value
			}
		}
	}

	return &ContainerStats{
		BlockInput:      blockInput,
		BlockOutput:     blockOutput,
		Container:       c.ID(),
		CPU:             cpu,
		CPUNano:         cpuNano,
		MemLimit:        memLimit,
		MemUsage:        memUsage,
		MemPerc:         memPerc,
		NetInput:        netInput,
		NetOutput:       netOutput,
		PIDs:            pids,
		SystemNano:      time.Now().UnixNano(),
		WorkingSetBytes: workingSetBytes,
	}
}

// SignalContainer sends a signal to a container process.
func (r *runtimeVM) SignalContainer(ctx context.Context, c *Container, sig syscall.Signal) error {
	log.Debugf(ctx, "RuntimeVM.SignalContainer() start")
	defer log.Debugf(ctx, "RuntimeVM.SignalContainer() end")

	// Lock the container
	c.opLock.Lock()
	defer c.opLock.Unlock()

	return r.kill(c.ID(), "", sig, true)
}

// AttachContainer attaches IO to a running container.
func (r *runtimeVM) AttachContainer(ctx context.Context, c *Container, inputStream io.Reader, outputStream, errorStream io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	log.Debugf(ctx, "RuntimeVM.AttachContainer() start")
	defer log.Debugf(ctx, "RuntimeVM.AttachContainer() end")

	// Initialize terminal resizing
	kubecontainer.HandleResizing(resize, func(size remotecommand.TerminalSize) {
		log.Debugf(ctx, "Got a resize event: %+v", size)

		if err := r.resizePty(c.ID(), "", size); err != nil {
			log.Warnf(ctx, "Failed to resize terminal: %v", err)
		}
	})

	r.Lock()
	cInfo, ok := r.ctrs[c.ID()]
	r.Unlock()
	if !ok {
		return errors.New("Could not retrieve container information")
	}

	opts := cio.AttachOptions{
		Stdin:     inputStream,
		Stdout:    outputStream,
		Stderr:    errorStream,
		Tty:       tty,
		StdinOnce: c.stdinOnce,
		CloseStdin: func() error {
			return r.closeIO(c.ID(), "")
		},
	}

	return cInfo.cio.Attach(opts)
}

// PortForwardContainer forwards the specified port provides statistics of a container.
func (r *runtimeVM) PortForwardContainer(ctx context.Context, c *Container, netNsPath string, port int32, stream io.ReadWriteCloser) error {
	log.Debugf(ctx, "RuntimeVM.PortForwardContainer() start")
	defer log.Debugf(ctx, "RuntimeVM.PortForwardContainer() end")

	return nil
}

// ReopenContainerLog reopens the log file of a container.
func (r *runtimeVM) ReopenContainerLog(ctx context.Context, c *Container) error {
	log.Debugf(ctx, "RuntimeVM.ReopenContainerLog() start")
	defer log.Debugf(ctx, "RuntimeVM.ReopenContainerLog() end")

	return nil
}

func (r *runtimeVM) WaitContainerStateStopped(ctx context.Context, c *Container) error {
	return nil
}

func (r *runtimeVM) start(ctrID, execID string) error {
	if _, err := r.task.Start(r.ctx, &task.StartRequest{
		ID:     ctrID,
		ExecID: execID,
	}); err != nil {
		return errdefs.FromGRPC(err)
	}

	return nil
}

func (r *runtimeVM) wait(ctrID, execID string) (int32, error) {
	resp, err := r.task.Wait(r.ctx, &task.WaitRequest{
		ID:     ctrID,
		ExecID: execID,
	})
	if err != nil {
		if !errors.Is(err, ttrpc.ErrClosed) {
			return -1, errdefs.FromGRPC(err)
		}
		return -1, errdefs.ErrNotFound
	}

	return int32(resp.ExitStatus), nil
}

func (r *runtimeVM) kill(ctrID, execID string, signal syscall.Signal, all bool) error {
	if _, err := r.task.Kill(r.ctx, &task.KillRequest{
		ID:     ctrID,
		ExecID: execID,
		Signal: uint32(signal),
		All:    all,
	}); err != nil {
		return errdefs.FromGRPC(err)
	}

	return nil
}

func (r *runtimeVM) remove(ctrID, execID string) error {
	if _, err := r.task.Delete(r.ctx, &task.DeleteRequest{
		ID:     ctrID,
		ExecID: execID,
	}); err != nil && !errors.Is(err, ttrpc.ErrClosed) {
		return errdefs.FromGRPC(err)
	}

	return nil
}

func (r *runtimeVM) resizePty(ctrID, execID string, size remotecommand.TerminalSize) error {
	_, err := r.task.ResizePty(r.ctx, &task.ResizePtyRequest{
		ID:     ctrID,
		ExecID: execID,
		Width:  uint32(size.Width),
		Height: uint32(size.Height),
	})
	if err != nil {
		return errdefs.FromGRPC(err)
	}

	return nil
}

func (r *runtimeVM) closeIO(ctrID, execID string) error {
	_, err := r.task.CloseIO(r.ctx, &task.CloseIORequest{
		ID:     ctrID,
		ExecID: execID,
		Stdin:  true,
	})
	if err != nil {
		return errdefs.FromGRPC(err)
	}

	return nil
}

// CheckpointContainer not implemented for runtimeVM
func (r *runtimeVM) CheckpointContainer(ctx context.Context, c *Container, specgen *rspec.Spec, leaveRunning bool) error {
	logrus.Debug("runtimeVM.CheckpointContainer() start")
	defer logrus.Debug("runtimeVM.CheckpointContainer() end")

	return errors.New("Checkpointing not implemented for runtimeVM")
}

// RestoreContainer not implemented for runtimeVM
func (r *runtimeVM) RestoreContainer(ctx context.Context, c *Container, sbSpec *rspec.Spec, infraPid int, cgroupParent string) error {
	logrus.Debug("runtimeVM.RestoreContainer() start")
	defer logrus.Debug("runtimeVM.RestoreContainer() end")

	return errors.New("Restoring not implemented for runtimeVM")
}
