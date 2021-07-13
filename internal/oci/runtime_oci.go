package oci

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	conmonconfig "github.com/containers/conmon/runner/config"
	"github.com/containers/podman/v3/pkg/annotations"
	"github.com/containers/podman/v3/pkg/checkpoint/crutils"
	"github.com/containers/storage/pkg/pools"
	"github.com/cri-o/cri-o/internal/log"
	"github.com/cri-o/cri-o/pkg/config"
	"github.com/cri-o/cri-o/pkg/criu"
	"github.com/cri-o/cri-o/server/cri/types"
	"github.com/cri-o/cri-o/server/metrics"
	"github.com/cri-o/cri-o/utils"
	"github.com/fsnotify/fsnotify"
	json "github.com/json-iterator/go"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
	kwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/remotecommand"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	utilexec "k8s.io/utils/exec"
)

const (
	// RuntimeTypeOCI is the type representing the RuntimeOCI implementation.
	RuntimeTypeOCI = "oci"

	// Command line flag used to specify the run root directory
	rootFlag = "--root"
)

// runtimeOCI is the Runtime interface implementation relying on conmon to
// interact with the container runtime.
type runtimeOCI struct {
	*Runtime

	path string
	root string
}

// newRuntimeOCI creates a new runtimeOCI instance
func newRuntimeOCI(r *Runtime, handler *config.RuntimeHandler) RuntimeImpl {
	runRoot := config.DefaultRuntimeRoot
	if handler.RuntimeRoot != "" {
		runRoot = handler.RuntimeRoot
	}

	return &runtimeOCI{
		Runtime: r,
		path:    handler.RuntimePath,
		root:    runRoot,
	}
}

// syncInfo is used to return data from monitor process to daemon
type syncInfo struct {
	Pid     int    `json:"pid"`
	Message string `json:"message,omitempty"`
}

// CreateContainer creates a container.
func (r *runtimeOCI) CreateContainer(ctx context.Context, c *Container, cgroupParent string, restore bool) (retErr error) {
	if c.Spoofed() {
		return nil
	}

	var stderrBuf bytes.Buffer
	parentPipe, childPipe, err := newPipe()
	if err != nil {
		return fmt.Errorf("error creating socket pair: %v", err)
	}
	childStartPipe, parentStartPipe, err := newPipe()
	if err != nil {
		return fmt.Errorf("error creating socket pair: %v", err)
	}
	defer parentPipe.Close()
	defer parentStartPipe.Close()

	args := []string{
		"-b", c.bundlePath,
		"-c", c.id,
		"--exit-dir", r.config.ContainerExitsDir,
		"-l", c.logPath,
		"--log-level", logrus.GetLevel().String(),
		"-n", c.name,
		"-P", c.conmonPidFilePath(),
		"-p", filepath.Join(c.bundlePath, "pidfile"),
		"--persist-dir", c.dir,
		"-r", r.path,
		"--runtime-arg", fmt.Sprintf("%s=%s", rootFlag, r.root),
		"--socket-dir-path", r.config.ContainerAttachSocketDir,
		"-u", c.id,
	}

	if r.config.CgroupManager().IsSystemd() {
		args = append(args, "-s")
	} else {
		args = append(args, "--syslog")
	}
	if r.config.LogSizeMax >= 0 {
		args = append(args, "--log-size-max", fmt.Sprintf("%v", r.config.LogSizeMax))
	}
	if r.config.LogToJournald {
		args = append(args, "--log-path", "journald:")
	}
	if r.config.NoPivot {
		args = append(args, "--no-pivot")
	}
	if c.terminal {
		args = append(args, "-t")
	} else if c.stdin {
		if !c.stdinOnce {
			args = append(args, "--leave-stdin-open")
		}
		args = append(args, "-i")
	}
	if restore {
		logrus.Debugf("Restore is true %v", restore)
		args = append(args, "--restore", c.CheckpointPath())
		if c.Spec().Process.SelinuxLabel != "" {
			args = append(
				args,
				"--runtime-opt",
				fmt.Sprintf(
					"--lsm-profile=selinux:%s",
					c.Spec().Process.SelinuxLabel,
				),
			)
		}
		if c.Spec().Linux.MountLabel != "" {
			args = append(
				args,
				"--runtime-opt",
				fmt.Sprintf(
					"--mount-context=%s",
					c.Spec().Linux.MountLabel,
				),
			)
		}
	}

	logrus.WithFields(logrus.Fields{
		"args": args,
	}).Debugf("running conmon: %s", r.config.Conmon)

	cmd := exec.Command(r.config.Conmon, args...) // nolint: gosec
	cmd.Dir = c.bundlePath
	cmd.SysProcAttr = sysProcAttrPlatform()
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if c.terminal {
		cmd.Stderr = &stderrBuf
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, childPipe, childStartPipe)
	// 0, 1 and 2 are stdin, stdout and stderr
	cmd.Env = r.config.ConmonEnv
	cmd.Env = append(cmd.Env,
		fmt.Sprintf("_OCI_SYNCPIPE=%d", 3),
		fmt.Sprintf("_OCI_STARTPIPE=%d", 4))
	if v, found := os.LookupEnv("XDG_RUNTIME_DIR"); found {
		cmd.Env = append(cmd.Env, fmt.Sprintf("XDG_RUNTIME_DIR=%s", v))
	}

	err = cmd.Start()
	if err != nil {
		childPipe.Close()
		childStartPipe.Close()
		return err
	}

	// We don't need childPipe on the parent side
	childPipe.Close()
	childStartPipe.Close()

	// Platform specific container setup
	if err := r.createContainerPlatform(c, cgroupParent, cmd.Process.Pid); err != nil {
		return err
	}

	/* We set the cgroup, now the child can start creating children */
	someData := []byte{0}
	_, err = parentStartPipe.Write(someData)
	if err != nil {
		if waitErr := cmd.Wait(); waitErr != nil {
			return errors.Wrap(err, waitErr.Error())
		}
		return err
	}

	/* Wait for initial setup and fork, and reap child */
	err = cmd.Wait()
	if err != nil {
		return err
	}

	// We will delete all container resources if creation fails
	defer func() {
		if retErr != nil {
			if err := r.DeleteContainer(ctx, c); err != nil {
				log.Warnf(ctx, "Unable to delete container %s: %v", c.ID(), err)
			}
		}
	}()

	// Wait to get container pid from conmon
	type syncStruct struct {
		si  *syncInfo
		err error
	}
	ch := make(chan syncStruct)
	go func() {
		var si *syncInfo
		if err = json.NewDecoder(parentPipe).Decode(&si); err != nil {
			ch <- syncStruct{err: err}
			return
		}
		ch <- syncStruct{si: si}
		close(ch)
	}()

	var pid int
	select {
	case ss := <-ch:
		if ss.err != nil {
			return fmt.Errorf("error reading container (probably exited) json message: %v", ss.err)
		}
		log.Debugf(ctx, "Received container pid: %d", ss.si.Pid)
		pid = ss.si.Pid
		if ss.si.Pid == -1 {
			if ss.si.Message != "" {
				log.Errorf(ctx, "Container creation error: %s", ss.si.Message)
				return fmt.Errorf("container create failed: %s", ss.si.Message)
			}
			log.Errorf(ctx, "Container creation failed")
			return fmt.Errorf("container create failed")
		}
	case <-time.After(ContainerCreateTimeout):
		log.Errorf(ctx, "Container creation timeout (%v)", ContainerCreateTimeout)
		return fmt.Errorf("create container timeout")
	}

	// Now we know the container has started, save the pid to verify against future calls.
	if err := c.state.SetInitPid(pid); err != nil {
		return err
	}

	return nil
}

// StartContainer starts a container.
func (r *runtimeOCI) StartContainer(ctx context.Context, c *Container) error {
	c.opLock.Lock()
	defer c.opLock.Unlock()

	if c.Spoofed() {
		return nil
	}

	if _, err := utils.ExecCmd(
		r.path, rootFlag, r.root, "start", c.id,
	); err != nil {
		return err
	}
	c.state.Started = time.Now()
	return nil
}

// ExecContainer prepares a streaming endpoint to execute a command in the container.
func (r *runtimeOCI) ExecContainer(ctx context.Context, c *Container, cmd []string, stdin io.Reader, stdout, stderr io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	if c.Spoofed() {
		return nil
	}

	processFile, err := prepareProcessExec(c, cmd, tty)
	if err != nil {
		return err
	}
	defer os.RemoveAll(processFile)

	execCmd := r.constructExecCommand(ctx, c, processFile, "")
	var cmdErr, copyError error
	if tty {
		cmdErr = ttyCmd(execCmd, stdin, stdout, resize)
	} else {
		var r, w *os.File
		if stdin != nil {
			// Use an os.Pipe here as it returns true *os.File objects.
			// This way, if you run 'kubectl exec <pod> -i bash' (no tty) and type 'exit',
			// the call below to execCmd.Run() can unblock because its Stdin is the read half
			// of the pipe.
			r, w, err = os.Pipe()
			if err != nil {
				return err
			}
			execCmd.Stdin = r
			go func() {
				_, copyError = pools.Copy(w, stdin)
				w.Close()
			}()
		}

		if stdout != nil {
			execCmd.Stdout = stdout
		}

		if stderr != nil {
			execCmd.Stderr = stderr
		}

		if err := execCmd.Start(); err != nil {
			return err
		}

		// The read side of the pipe should be closed after the container process has been started.
		if r != nil {
			if err := r.Close(); err != nil {
				return err
			}
		}

		cmdErr = execCmd.Wait()
	}

	if copyError != nil {
		return copyError
	}
	if exitErr, ok := cmdErr.(*exec.ExitError); ok {
		return &utilexec.ExitErrorWrapper{ExitError: exitErr}
	}
	return cmdErr
}

// ExecSyncContainer execs a command in a container and returns it's stdout, stderr and return code.
func (r *runtimeOCI) ExecSyncContainer(ctx context.Context, c *Container, command []string, timeout int64) (*types.ExecSyncResponse, error) {
	if c.Spoofed() {
		return nil, nil
	}

	processFile, err := prepareProcessExec(c, command, c.terminal)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(processFile)

	pidDir, err := ioutil.TempDir("", "pidfile")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(pidDir)

	pidFile := filepath.Join(pidDir, c.id)

	cmd := r.constructExecCommand(ctx, c, processFile, pidFile)
	cmd.SysProcAttr = sysProcAttrPlatform()

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	err = cmd.Start()
	if err != nil {
		return nil, err
	}

	// wait till the command is done
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	if timeout > 0 {
		select {
		case <-time.After(time.Second * time.Duration(timeout)):
			// Ensure the process is not left behind
			killContainerExecProcess(ctx, pidFile, cmd)

			// Make sure the runtime process has been cleaned up
			<-done

			// If the command timed out, we should return an ExecSyncResponse with a non-zero exit code because
			// the prober code in the kubelet checks for it. If we return a custom error,
			// then the probes transition into Unknown status and the container isn't restarted as expected.
			return &types.ExecSyncResponse{
				Stderr:   []byte(conmonconfig.TimedOutMessage),
				ExitCode: -1,
			}, nil
		case err = <-done:
			break
		}
	} else {
		err = <-done
	}

	// gather exit code from err
	exitCode := int32(0)
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode = int32(exitError.ExitCode())
		}
	}

	return &types.ExecSyncResponse{
		Stdout:   stdoutBuf.Bytes(),
		Stderr:   stderrBuf.Bytes(),
		ExitCode: exitCode,
	}, nil
}

func (r *runtimeOCI) constructExecCommand(ctx context.Context, c *Container, processFile, pidFile string) *exec.Cmd {
	args := []string{rootFlag, r.root, "exec"}
	if pidFile != "" {
		args = append(args, "--pid-file", pidFile)
	}
	args = append(args, "--process", processFile, c.ID())
	execCmd := exec.CommandContext(ctx, r.path, args...) // nolint: gosec
	if v, found := os.LookupEnv("XDG_RUNTIME_DIR"); found {
		execCmd.Env = append(execCmd.Env, fmt.Sprintf("XDG_RUNTIME_DIR=%s", v))
	}
	return execCmd
}

func killContainerExecProcess(ctx context.Context, pidFile string, cmd *exec.Cmd) {
	// Attempt to get the container PID and PGID from the file the runtime should have written.
	ctrPid, ctrPgid, err := pidAndpgidFromFile(pidFile)
	if err != nil && ctrPid <= 0 {
		// only kill the runtime process if we failed to find a ctrPid
		// as this means the runtime exec hasn't successfully written the pid file
		if killErr := cmd.Process.Kill(); killErr != nil {
			log.Errorf(ctx, "Error killing runtime exec process(%v) after error finding runtime pid: (%v)", killErr, err)
		}
	}

	if ctrPgid > 1 {
		// First attempt to kill the container group
		if err := syscall.Kill(-ctrPgid, syscall.SIGKILL); err != nil {
			log.Errorf(ctx, "Failed to kill process group after timeout: %v", err)
		}
	} else if ctrPid > 0 {
		// If that fails, kill the container PID itself
		if err := syscall.Kill(ctrPid, syscall.SIGKILL); err != nil {
			log.Errorf(ctx, "Failed to kill process after timeout: %v", err)
		}
	}
}

func pidAndpgidFromFile(pidFile string) (pid, pgid int, _ error) {
	// find the pid of the parent process
	pidStr, err := ioutil.ReadFile(pidFile)
	if err != nil {
		return -1, -1, err
	}
	pid, err = strconv.Atoi(string(pidStr))
	if err != nil {
		return -1, -1, err
	}
	pgid, err = syscall.Getpgid(pid)
	return pid, pgid, err
}

// UpdateContainer updates container resources
func (r *runtimeOCI) UpdateContainer(ctx context.Context, c *Container, res *rspec.LinuxResources) error {
	if c.Spoofed() {
		return nil
	}

	cmd := exec.Command(r.path, rootFlag, r.root, "update", "--resources", "-", c.id) // nolint: gosec
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if v, found := os.LookupEnv("XDG_RUNTIME_DIR"); found {
		cmd.Env = append(cmd.Env, fmt.Sprintf("XDG_RUNTIME_DIR=%s", v))
	}
	jsonResources, err := json.Marshal(res)
	if err != nil {
		return err
	}
	cmd.Stdin = bytes.NewReader(jsonResources)

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("updating resources for container %q failed: %v %v (%v)", c.id, stderr.String(), stdout.String(), err)
	}
	return nil
}

func WaitContainerStop(ctx context.Context, c *Container, timeout time.Duration, ignoreKill bool) error {
	done := make(chan struct{})
	// we could potentially re-use "done" channel to exit the loop on timeout,
	// but we use another channel "chControl" so that we never panic
	// attempting to close an already-closed "done" channel.  The panic
	// would occur in the "default" select case below if we'd closed the
	// "done" channel (instead of the "chControl" channel) in the timeout
	// select case.
	chControl := make(chan struct{})
	go func() {
		for {
			select {
			case <-chControl:
				close(done)
				return
			default:
				if err := c.verifyPid(); err != nil {
					// The initial container process either doesn't exist, or isn't ours.
					if !errors.Is(err, ErrNotFound) {
						log.Warnf(ctx, "Failed to find process for container %s: %v", c.id, err)
					}
					close(done)
					return
				}
				// the PID is still active and belongs to the container, continue to wait
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()
	// Operate in terms of targetTime, so that we can pause in the middle of the operation
	// to catch a new timeout (and possibly ignore that new timeout if it's not correct to
	// take a new one).
	targetTime := time.Now().Add(timeout)
	killed := false
	for !killed {
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			close(chControl)
			return ctx.Err()
		case <-time.After(time.Until(targetTime)):
			close(chControl)
			if ignoreKill {
				return fmt.Errorf("timeout reached after %.0f seconds waiting for container process to exit",
					timeout.Seconds())
			}
			pid, err := c.pid()
			if err != nil {
				return err
			}
			if err := Kill(pid); err != nil {
				return fmt.Errorf("failed to kill process: %v", err)
			}
			killed = true
		case newTimeout := <-c.stopTimeoutChan:
			// If a new timeout comes in,
			// interrupt the old one, and start a new one
			newTargetTime := time.Now().Add(newTimeout)

			// but only if it's earlier
			if !newTargetTime.Before(targetTime) {
				continue
			}

			targetTime = newTargetTime
			timeout = newTimeout
		}
	}
	c.state.Finished = time.Now()
	// Successfully stopped! This is to prevent other routines from
	// racing with this one and waiting forever.
	// Close only the dedicated channel. If we close stopTimeoutChan,
	// any other waiting goroutine will panic, not gracefully exit.
	close(c.stoppedChan)
	return nil
}

// StopContainer stops a container. Timeout is given in seconds.
func (r *runtimeOCI) StopContainer(ctx context.Context, c *Container, timeout int64) (retErr error) {
	c.SetAsStopping(timeout)
	defer func() {
		if retErr != nil {
			// Failed to stop, set stopping to false.
			// Otherwise, we won't actually
			// attempt to stop when a new request comes in,
			// even though we're not actively stopping anymore.
			c.SetAsNotStopping()
		}
	}()

	c.opLock.Lock()
	defer c.opLock.Unlock()

	if err := c.ShouldBeStopped(); err != nil {
		return err
	}

	if c.Spoofed() {
		c.state.Status = ContainerStateStopped
		c.state.Finished = time.Now()
		return nil
	}

	// The initial container process either doesn't exist, or isn't ours.
	if err := c.verifyPid(); err != nil {
		c.state.Finished = time.Now()
		return nil
	}

	if timeout > 0 {
		if _, err := utils.ExecCmd(
			r.path, rootFlag, r.root, "kill", c.id, c.GetStopSignal(),
		); err != nil {
			checkProcessGone(c)
		}
		err := WaitContainerStop(ctx, c, time.Duration(timeout)*time.Second, true)
		if err == nil {
			return nil
		}
		log.Warnf(ctx, "Stopping container %v with stop signal timed out: %v", c.id, err)
	}

	if _, err := utils.ExecCmd(
		r.path, rootFlag, r.root, "kill", c.id, "KILL",
	); err != nil {
		checkProcessGone(c)
	}

	return WaitContainerStop(ctx, c, killContainerTimeout, false)
}

func checkProcessGone(c *Container) {
	if err := c.verifyPid(); err != nil {
		// The initial container process either doesn't exist, or isn't ours.
		// Set state accordingly.
		c.state.Finished = time.Now()
	}
}

// DeleteContainer deletes a container.
func (r *runtimeOCI) DeleteContainer(ctx context.Context, c *Container) error {
	c.opLock.Lock()
	defer c.opLock.Unlock()

	if c.Spoofed() {
		return nil
	}

	_, err := utils.ExecCmd(r.path, rootFlag, r.root, "delete", "--force", c.id)
	return err
}

func updateContainerStatusFromExitFile(c *Container) error {
	exitFilePath := c.exitFilePath()
	fi, err := os.Stat(exitFilePath)
	if err != nil {
		return errors.Wrapf(err, "failed to find container exit file for %s", c.id)
	}
	c.state.Finished, err = getFinishedTime(fi)
	if err != nil {
		return errors.Wrap(err, "failed to get finished time")
	}
	statusCodeStr, err := ioutil.ReadFile(exitFilePath)
	if err != nil {
		return errors.Wrap(err, "failed to read exit file")
	}
	statusCode, err := strconv.Atoi(string(statusCodeStr))
	if err != nil {
		return errors.Wrap(err, "status code conversion failed")
	}
	c.state.ExitCode = utils.Int32Ptr(int32(statusCode))
	return nil
}

// UpdateContainerStatus refreshes the status of the container.
func (r *runtimeOCI) UpdateContainerStatus(ctx context.Context, c *Container) error {
	c.opLock.Lock()
	defer c.opLock.Unlock()

	if c.Spoofed() {
		return nil
	}

	if c.state.ExitCode != nil && !c.state.Finished.IsZero() {
		log.Debugf(ctx, "Skipping status update for: %+v", c.state)
		return nil
	}

	stateCmd := func() (*ContainerState, bool, error) {
		cmd := exec.Command(r.path, rootFlag, r.root, "state", c.id) // nolint: gosec
		if v, found := os.LookupEnv("XDG_RUNTIME_DIR"); found {
			cmd.Env = append(cmd.Env, fmt.Sprintf("XDG_RUNTIME_DIR=%s", v))
		}
		out, err := cmd.Output()
		if err != nil {
			// there are many code paths that could lead to have a bad state in the
			// underlying runtime.
			// On any error like a container went away or we rebooted and containers
			// went away we do not error out stopping kubernetes to recover.
			// We always populate the fields below so kube can restart/reschedule
			// containers failing.
			if exitErr, isExitError := err.(*exec.ExitError); isExitError {
				log.Errorf(ctx, "Failed to update container state for %s: stdout: %s, stderr: %s", c.id, string(out), string(exitErr.Stderr))
			} else {
				log.Errorf(ctx, "Failed to update container state for %s: %v", c.id, err)
			}
			c.state.Status = ContainerStateStopped
			if err := updateContainerStatusFromExitFile(c); err != nil {
				c.state.Finished = time.Now()
				c.state.ExitCode = utils.Int32Ptr(255)
			}
			return nil, true, nil
		}
		state := *c.state
		if err := json.NewDecoder(bytes.NewBuffer(out)).Decode(&state); err != nil {
			return &state, false, fmt.Errorf("failed to decode container status for %s: %s", c.id, err)
		}
		return &state, false, nil
	}
	state, canReturn, err := stateCmd()
	if err != nil {
		return err
	}
	if canReturn {
		return nil
	}

	if state.Status != ContainerStateStopped {
		*c.state = *state
		return nil
	}
	// release the lock before waiting
	c.opLock.Unlock()
	exitFilePath := c.exitFilePath()
	var fi os.FileInfo
	err = kwait.ExponentialBackoff(
		kwait.Backoff{
			Duration: 500 * time.Millisecond,
			Factor:   1.2,
			Steps:    6,
		},
		func() (bool, error) {
			var err error
			fi, err = os.Stat(exitFilePath)
			if err != nil {
				// wait longer
				return false, nil
			}
			return true, nil
		})
	c.opLock.Lock()
	// run command again
	state, _, err2 := stateCmd()
	if err2 != nil {
		return err2
	}
	if state == nil {
		return fmt.Errorf("state command returned nil")
	}
	*c.state = *state
	if err != nil {
		log.Warnf(ctx, "Failed to find container exit file for %v: %v", c.id, err)
	} else {
		c.state.Finished, err = getFinishedTime(fi)
		if err != nil {
			return fmt.Errorf("failed to get finished time: %v", err)
		}
		statusCodeStr, err := ioutil.ReadFile(exitFilePath)
		if err != nil {
			return errors.Wrap(err, "failed to read exit file: %v")
		}
		statusCode, err := strconv.Atoi(string(statusCodeStr))
		if err != nil {
			return fmt.Errorf("status code conversion failed: %v", err)
		}
		c.state.ExitCode = utils.Int32Ptr(int32(statusCode))
		log.Debugf(ctx, "Found exit code for %s: %d", c.id, statusCode)
	}

	oomFilePath := filepath.Join(c.bundlePath, "oom")
	if _, err = os.Stat(oomFilePath); err == nil {
		c.state.OOMKilled = true

		// Collect total metric
		metrics.Instance().MetricContainersOOMTotalInc()

		// Collect metric by container name
		metrics.Instance().MetricContainersOOMInc(c.Name())
	}

	return nil
}

// PauseContainer pauses a container.
func (r *runtimeOCI) PauseContainer(ctx context.Context, c *Container) error {
	c.opLock.Lock()
	defer c.opLock.Unlock()

	if c.Spoofed() {
		return nil
	}

	_, err := utils.ExecCmd(r.path, rootFlag, r.root, "pause", c.id)
	return err
}

// UnpauseContainer unpauses a container.
func (r *runtimeOCI) UnpauseContainer(ctx context.Context, c *Container) error {
	c.opLock.Lock()
	defer c.opLock.Unlock()

	if c.Spoofed() {
		return nil
	}

	_, err := utils.ExecCmd(r.path, rootFlag, r.root, "resume", c.id)
	return err
}

func (r *runtimeOCI) WaitContainerStateStopped(ctx context.Context, c *Container) error {
	return nil
}

// ContainerStats provides statistics of a container.
func (r *runtimeOCI) ContainerStats(ctx context.Context, c *Container, cgroup string) (*ContainerStats, error) {
	c.opLock.Lock()
	defer c.opLock.Unlock()
	return r.containerStats(c, cgroup)
}

// SignalContainer sends a signal to a container process.
func (r *runtimeOCI) SignalContainer(ctx context.Context, c *Container, sig syscall.Signal) error {
	c.opLock.Lock()
	defer c.opLock.Unlock()

	if c.Spoofed() {
		return nil
	}

	if unix.SignalName(sig) == "" {
		return errors.Errorf("unable to find signal %s", sig.String())
	}

	_, err := utils.ExecCmd(
		r.path, rootFlag, r.root, "kill", c.ID(), strconv.Itoa(int(sig)),
	)
	return err
}

// AttachContainer attaches IO to a running container.
func (r *runtimeOCI) AttachContainer(ctx context.Context, c *Container, inputStream io.Reader, outputStream, errorStream io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	if c.Spoofed() {
		return nil
	}

	controlPath := filepath.Join(c.BundlePath(), "ctl")
	controlFile, err := os.OpenFile(controlPath, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("failed to open container ctl file: %v", err)
	}
	defer controlFile.Close()

	kubecontainer.HandleResizing(resize, func(size remotecommand.TerminalSize) {
		log.Debugf(ctx, "Got a resize event: %+v", size)
		_, err := fmt.Fprintf(controlFile, "%d %d %d\n", 1, size.Height, size.Width)
		if err != nil {
			log.Debugf(ctx, "Failed to write to control file to resize terminal: %v", err)
		}
	})

	attachSocketPath := filepath.Join(r.config.ContainerAttachSocketDir, c.ID(), "attach")
	conn, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: attachSocketPath, Net: "unixpacket"})
	if err != nil {
		return fmt.Errorf("failed to connect to container %s attach socket: %v", c.ID(), err)
	}
	defer conn.Close()

	receiveStdout := make(chan error)
	if outputStream != nil || errorStream != nil {
		go func() {
			receiveStdout <- redirectResponseToOutputStreams(outputStream, errorStream, conn)
			close(receiveStdout)
		}()
	}

	stdinDone := make(chan error)
	go func() {
		var err, closeErr error
		if inputStream != nil {
			_, err = utils.CopyDetachable(conn, inputStream, nil)
			closeErr = conn.CloseWrite()
		}
		switch {
		case err != nil:
			stdinDone <- err
		case closeErr != nil:
			stdinDone <- closeErr
		default:
			// neither CopyDetachable nor CloseWrite returned error
			stdinDone <- nil
		}
		close(stdinDone)
	}()

	select {
	case err := <-receiveStdout:
		return err
	case err := <-stdinDone:
		// This particular case is for when we get a non-tty attach
		// with --leave-stdin-open=true. We want to return as soon
		// as we receive EOF from the client. However, we should do
		// this only when stdin is enabled. If there is no stdin
		// enabled then we wait for output as usual.
		if c.stdin && !c.StdinOnce() && !tty {
			return nil
		}
		if _, ok := err.(utils.DetachError); ok {
			return nil
		}
		if outputStream != nil || errorStream != nil {
			return <-receiveStdout
		}
	}

	return nil
}

// PortForwardContainer forwards the specified port into the provided container.
func (r *runtimeOCI) PortForwardContainer(ctx context.Context, c *Container, netNsPath string, port int32, stream io.ReadWriteCloser) error {
	log.Infof(ctx,
		"Starting port forward for %s in network namespace %s", c.ID(), netNsPath,
	)

	// Adapted reference implementation:
	// https://github.com/containerd/cri/blob/8c366d/pkg/server/sandbox_portforward_unix.go#L65-L120
	if err := ns.WithNetNSPath(netNsPath, func(_ ns.NetNS) error {
		defer stream.Close()

		// localhost can resolve to both IPv4 and IPv6 addresses in dual-stack systems
		// but the application can be listening in one of the IP families only.
		// golang has enabled RFC 6555 Fast Fallback (aka HappyEyeballs) by default in 1.12
		// It means that if a host resolves to both IPv6 and IPv4, it will try to connect to any
		// of those addresses and use the working connection.
		// xref https://github.com/golang/go/commit/efc185029bf770894defe63cec2c72a4c84b2ee9
		// However, the implementation uses go routines to start both connections in parallel,
		// and this has limitations when running inside a namespace, so we try to the connections
		// serially disabling the Fast Fallback support.
		// xref https://github.com/golang/go/issues/44922
		var d net.Dialer
		d.FallbackDelay = -1
		conn, err := d.Dial("tcp", fmt.Sprintf("localhost:%d", port))
		if err != nil {
			return errors.Wrapf(err, "failed to connect to localhost:%d inside namespace %s", port, c.ID())
		}
		defer conn.Close()

		errCh := make(chan error, 2)

		debug := func(format string, args ...interface{}) {
			log.Debugf(ctx, fmt.Sprintf(
				"PortForward (id: %s, port: %d): %s", c.ID(), port, format,
			), args...)
		}

		// Copy from the the namespace port connection to the client stream
		go func() {
			debug("copy data from container to client")
			_, err := io.Copy(stream, conn)
			errCh <- err
		}()

		// Copy from the client stream to the namespace port connection
		go func() {
			debug("copy data from client to container")
			_, err := io.Copy(conn, stream)
			errCh <- err
		}()

		// Wait until the first error is returned by one of the connections we
		// use errFwd to store the result of the port forwarding operation if
		// the context is cancelled close everything and return
		var errFwd error
		select {
		case errFwd = <-errCh:
			debug("stop forwarding in direction: %v", errFwd)
		case <-ctx.Done():
			debug("cancelled: %v", ctx.Err())
			return ctx.Err()
		}

		// give a chance to terminate gracefully or timeout
		const timeout = time.Second
		select {
		case e := <-errCh:
			if errFwd == nil {
				errFwd = e
			}
			debug("stopped forwarding in both directions")

		case <-time.After(timeout):
			debug("timed out waiting to close the connection")

		case <-ctx.Done():
			debug("cancelled: %v", ctx.Err())
			errFwd = ctx.Err()
		}

		return errFwd
	}); err != nil {
		return errors.Wrapf(
			err, "port forward into network namespace %q", netNsPath,
		)
	}

	log.Infof(ctx, "Finished port forwarding for %q on port %d", c.ID(), port)
	return nil
}

// ReopenContainerLog reopens the log file of a container.
func (r *runtimeOCI) ReopenContainerLog(ctx context.Context, c *Container) error {
	if c.Spoofed() {
		return nil
	}

	controlPath := filepath.Join(c.BundlePath(), "ctl")
	controlFile, err := os.OpenFile(controlPath, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("failed to open container ctl file: %v", err)
	}
	defer controlFile.Close()

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create new watch: %v", err)
	}
	defer watcher.Close()

	done := make(chan struct{})
	doneClosed := false
	errorCh := make(chan error)
	go func() {
		for {
			select {
			case event := <-watcher.Events:
				log.Debugf(ctx, "Event: %v", event)
				if event.Op&fsnotify.Create == fsnotify.Create || event.Op&fsnotify.Write == fsnotify.Write {
					log.Debugf(ctx, "File created %s", event.Name)
					if event.Name == c.LogPath() {
						log.Debugf(ctx, "Expected log file created")
						done <- struct{}{}
						return
					}
				}
			case err := <-watcher.Errors:
				errorCh <- fmt.Errorf("watch error for container log reopen %v: %v", c.ID(), err)
				close(errorCh)
				return
			}
		}
	}()
	cLogDir := filepath.Dir(c.LogPath())
	if err := watcher.Add(cLogDir); err != nil {
		log.Errorf(ctx, "Watcher.Add(%q) failed: %s", cLogDir, err)
		close(done)
		doneClosed = true
	}

	if _, err = fmt.Fprintf(controlFile, "%d %d %d\n", 2, 0, 0); err != nil {
		log.Debugf(ctx, "Failed to write to control file to reopen log file: %v", err)
	}

	select {
	case err := <-errorCh:
		if !doneClosed {
			close(done)
		}
		return err
	case <-done:
		if !doneClosed {
			close(done)
		}
		break
	}

	return nil
}

// prepareProcessExec returns the path of the process.json used in runc exec -p
// caller is responsible for removing the returned file, if prepareProcessExec succeeds
func prepareProcessExec(c *Container, cmd []string, tty bool) (processFile string, retErr error) {
	f, err := ioutil.TempFile("", "exec-process-")
	if err != nil {
		return "", err
	}
	f.Close()
	processFile = f.Name()
	defer func() {
		if retErr != nil {
			os.RemoveAll(processFile)
		}
	}()

	// It's important to make a spec copy here to not overwrite the initial
	// process spec
	pspec := *c.Spec().Process
	pspec.Args = cmd
	// We need to default this to false else it will inherit terminal as true
	// from the container.
	pspec.Terminal = false
	if tty {
		pspec.Terminal = true
	}
	processJSON, err := json.Marshal(pspec)
	if err != nil {
		return "", err
	}

	if err := ioutil.WriteFile(processFile, processJSON, 0o644); err != nil {
		return "", err
	}
	return processFile, nil
}

// ReadConmonPidFile attempts to read conmon's pid from its pid file
// This function makes no verification that this file should exist
// it is up to the caller to verify that this container has a conmon
func ReadConmonPidFile(c *Container) (int, error) {
	contents, err := ioutil.ReadFile(c.conmonPidFilePath())
	if err != nil {
		return -1, err
	}
	// Convert it to an int
	conmonPID, err := strconv.Atoi(string(contents))
	if err != nil {
		return -1, err
	}
	return conmonPID, nil
}

func (c *Container) conmonPidFilePath() string {
	return filepath.Join(c.bundlePath, "conmon-pidfile")
}

// CheckpointContainer checkpoints a container.
func (r *runtimeOCI) CheckpointContainer(ctx context.Context, c *Container, specgen *rspec.Spec, leaveRunning bool) error {
	c.opLock.Lock()
	defer c.opLock.Unlock()

	if err := r.checkpointRestoreSupported(); err != nil {
		return err
	}

	// Once CRIU infects the process in the container with the
	// parasite, the parasite also wants to write to the log
	// file which is outside of the container. Giving the log file
	// the label of the container enables logging for the parasite.
	if err := crutils.CRCreateFileWithLabel(c.Dir(), "dump.log", specgen.Linux.MountLabel); err != nil {
		return err
	}

	// workPath will be used to store dump.log and stats-dump
	workPath := c.Dir()
	// imagePath is used by CRIU to store the actual checkpoint files
	imagePath := c.CheckpointPath()

	logrus.Debugf("Writing checkpoint to %s", imagePath)
	logrus.Debugf("Writing checkpoint logs to %s", workPath)
	args := []string{}
	args = append(
		args,
		"--criu",
		r.config.CriuPath,
		rootFlag,
		r.root,
		"checkpoint",
		"--image-path",
		imagePath,
		"--work-path",
		workPath,
	)
	if leaveRunning {
		args = append(args, "--leave-running")
	}

	args = append(args, c.id)

	_, err := utils.ExecCmd(r.path, args...)
	if err != nil {
		return errors.Wrapf(err, "Running %q %q failed", r.path, args)
	}

	if !leaveRunning {
		c.state.Status = ContainerStateStopped
		c.state.ExitCode = utils.Int32Ptr(0)
		c.state.Finished = time.Now()
	}

	return nil
}

// RestoreContainer restores a container.
func (r *runtimeOCI) RestoreContainer(ctx context.Context, c *Container, sbSpec *rspec.Spec, infraPid int, cgroupParent string) error {
	c.opLock.Lock()
	defer c.opLock.Unlock()

	if err := r.checkpointRestoreSupported(); err != nil {
		return err
	}

	// Let's try to stat() CRIU's inventory file. If it does not exist, it makes
	// no sense to try a restore. This is a minimal check if a checkpoint exist.
	if _, err := os.Stat(filepath.Join(c.CheckpointPath(), "inventory.img")); os.IsNotExist(err) {
		return errors.Wrapf(err, "A complete checkpoint for this container cannot be found, cannot restore")
	}

	// remove conmon files
	attachFile := filepath.Join(c.BundlePath(), "attach")
	if err := os.Remove(attachFile); err != nil && !os.IsNotExist(err) {
		return errors.Wrapf(err, "error removing container %s attach file", c.ID())
	}

	ctlFile := filepath.Join(c.BundlePath(), "ctl")
	if err := os.Remove(ctlFile); err != nil && !os.IsNotExist(err) {
		return errors.Wrapf(err, "error removing container %s ctl file", c.ID())
	}

	winszFile := filepath.Join(c.BundlePath(), "winsz")
	if err := os.Remove(winszFile); err != nil && !os.IsNotExist(err) {
		return errors.Wrapf(err, "error removing container %s winsz file", c.ID())
	}

	// Figure out if this container will be restored in another sandbox
	oldSbID := c.Sandbox()
	if oldSbID == "" {
		return fmt.Errorf("failed to detect sandbox of to be restored container %s", c.ID())
	}
	newSbID := sbSpec.Annotations[annotations.SandboxID]
	if newSbID == "" {
		return fmt.Errorf("failed to detect destination sandbox of to be restored container %s", c.ID())
	}

	// Get config.json to adapt for restore (mostly annotations for restore in another sandbox)
	configFile := filepath.Join(c.BundlePath(), "config.json")
	specgen, err := generate.NewFromFile(configFile)
	if err != nil {
		return err
	}

	if oldSbID != newSbID {
		// The container will be restored in another (not the original) sandbox
		// Adapt to namespaces of the new sandbox
		for i, n := range specgen.Config.Linux.Namespaces {
			if n.Path == "" {
				// The namespace in the original container did not point to
				// an existing interface. Leave it as it is.
				continue
			}
			for _, on := range sbSpec.Linux.Namespaces {
				if on.Type == n.Type {
					var nsPath string
					if n.Type == rspec.NetworkNamespace {
						// Type for network namespaces is 'network'.
						// The kernel link is 'net'.
						nsPath = fmt.Sprintf("/proc/%d/ns/%s", infraPid, "net")
					} else {
						nsPath = fmt.Sprintf("/proc/%d/ns/%s", infraPid, n.Type)
					}
					specgen.Config.Linux.Namespaces[i].Path = nsPath
					break
				}
			}
		}

		// Update Sandbox Name
		specgen.AddAnnotation(annotations.SandboxName, sbSpec.Annotations[annotations.Name])
		// Update Sandbox ID
		specgen.AddAnnotation(annotations.SandboxID, newSbID)

		// Update Name
		ctrMetadata := runtime.ContainerMetadata{}
		err = json.Unmarshal([]byte(sbSpec.Annotations[annotations.Metadata]), &ctrMetadata)
		if err != nil {
			return err
		}
		ctrName := ctrMetadata.Name

		podMetadata := runtime.PodSandboxMetadata{}
		err = json.Unmarshal([]byte(specgen.Config.Annotations[annotations.Metadata]), &podMetadata)
		if err != nil {
			return err
		}
		uid := podMetadata.Uid
		mData := fmt.Sprintf("k8s_%s_%s_%s_%s0", ctrName, sbSpec.Annotations[annotations.KubeName], sbSpec.Annotations[annotations.Namespace], uid)
		specgen.AddAnnotation(annotations.Name, mData)

		c.sandbox = newSbID

		saveOptions := generate.ExportOptions{}
		if err := specgen.SaveToFile(configFile, saveOptions); err != nil {
			return err
		}
	}

	c.state.InitPid = 0
	c.state.InitStartTime = ""

	// It is possible to tell runc to place the CRIU log files
	// at a custom location '--work-path'. But for restoring a
	// container we are not calling runc directly but conmon, which
	// then calls runc. It would be possible to change conmon to
	// also have the log file in the same location as during
	// checkpointing, but it is not really that important right now.
	if err := crutils.CRCreateFileWithLabel(c.BundlePath(), "restore.log", specgen.Config.Linux.MountLabel); err != nil {
		return err
	}

	if err := r.CreateContainer(ctx, c, cgroupParent, true); err != nil {
		return err
	}

	// Once the container is restored, update the metadata
	// 1. Container is running again
	c.state.Status = ContainerStateRunning
	// 2. Update PID of the container (without that stopping will fail)
	pid, err := ReadConmonPidFile(c)
	if err != nil {
		return err
	}
	c.state.Pid = pid
	// 3. Reset ExitCode (also needed for stopping)
	c.state.ExitCode = nil
	// 4. Set start time
	c.state.Started = time.Now()

	return nil
}

func (r *runtimeOCI) checkpointRestoreSupported() error {
	if !criu.CheckForCriu() {
		return errors.Errorf("checkpoint/restore requires at least CRIU %d", criu.MinCriuVersion)
	}
	if !crutils.CRRuntimeSupportsCheckpointRestore(r.path) {
		return errors.Errorf("configured runtime does not support checkpoint/restore")
	}
	return nil
}
