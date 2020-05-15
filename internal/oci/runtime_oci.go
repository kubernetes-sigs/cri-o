package oci

import (
	"bytes"
	"encoding/json"
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
	"github.com/containers/storage/pkg/pools"
	"github.com/cri-o/cri-o/internal/findprocess"
	"github.com/cri-o/cri-o/internal/log"
	"github.com/cri-o/cri-o/pkg/config"
	"github.com/cri-o/cri-o/utils"
	"github.com/fsnotify/fsnotify"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
	kwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/remotecommand"
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

// exitCodeInfo is used to return the monitored process exit code to the daemon
type exitCodeInfo struct {
	ExitCode int32  `json:"exit_code"`
	Message  string `json:"message,omitempty"`
}

// CreateContainer creates a container.
func (r *runtimeOCI) CreateContainer(c *Container, cgroupParent string) (retErr error) {
	var stderrBuf bytes.Buffer
	parentPipe, childPipe, err := newPipe()
	childStartPipe, parentStartPipe, err := newPipe()
	if err != nil {
		return fmt.Errorf("error creating socket pair: %v", err)
	}
	defer parentPipe.Close()
	defer parentStartPipe.Close()

	args := []string{
		"-c", c.id,
		"-n", c.name,
		"-u", c.id,
		"-r", r.path,
		"-b", c.bundlePath,
		"--persist-dir", c.dir,
		"-p", filepath.Join(c.bundlePath, "pidfile"),
		"-P", c.conmonPidFilePath(),
		"-l", c.logPath,
		"--exit-dir", r.config.ContainerExitsDir,
		"--socket-dir-path", r.config.ContainerAttachSocketDir,
		"--log-level", logrus.GetLevel().String(),
		"--runtime-arg", fmt.Sprintf("%s=%s", rootFlag, r.root),
		"--syslog",
	}
	if r.config.CgroupManager == SystemdCgroupsManager {
		args = append(args, "-s")
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
			if err := r.DeleteContainer(c); err != nil {
				logrus.Warnf("unable to delete container %s: %v", c.ID(), err)
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

	select {
	case ss := <-ch:
		if ss.err != nil {
			return fmt.Errorf("error reading container (probably exited) json message: %v", ss.err)
		}
		logrus.Debugf("Received container pid: %d", ss.si.Pid)
		if ss.si.Pid == -1 {
			if ss.si.Message != "" {
				logrus.Errorf("Container creation error: %s", ss.si.Message)
				return fmt.Errorf("container create failed: %s", ss.si.Message)
			}
			logrus.Errorf("Container creation failed")
			return fmt.Errorf("container create failed")
		}
	case <-time.After(ContainerCreateTimeout):
		logrus.Errorf("Container creation timeout (%v)", ContainerCreateTimeout)
		return fmt.Errorf("create container timeout")
	}

	return nil
}

// StartContainer starts a container.
func (r *runtimeOCI) StartContainer(c *Container) error {
	c.opLock.Lock()
	defer c.opLock.Unlock()

	if _, err := utils.ExecCmd(
		r.path, rootFlag, r.root, "start", c.id,
	); err != nil {
		return err
	}
	c.state.Started = time.Now()
	return nil
}

func prepareExec() (pidFile, parentPipe, childPipe *os.File, _ error) {
	var err error
	parentPipe, childPipe, err = os.Pipe()
	if err != nil {
		return nil, nil, nil, err
	}

	pidFile, err = ioutil.TempFile("", "pidfile")
	if err != nil {
		parentPipe.Close()
		childPipe.Close()
		return nil, nil, nil, err
	}

	return
}

func parseLog(l []byte) (stdout, stderr []byte) {
	// Split the log on newlines, which is what separates entries.
	lines := bytes.SplitAfter(l, []byte{'\n'})
	for _, line := range lines {
		// Ignore empty lines.
		if len(line) == 0 {
			continue
		}

		// The format of log lines is "DATE pipe LogTag REST".
		parts := bytes.SplitN(line, []byte{' '}, 4)
		if len(parts) < 4 {
			// Ignore the line if it's formatted incorrectly, but complain
			// about it so it can be debugged.
			logrus.Warnf("hit invalid log format: %q", string(line))
			continue
		}

		pipe := string(parts[1])
		content := parts[3]

		linetype := string(parts[2])
		if linetype == "P" {
			contentLen := len(content)
			if contentLen > 0 && content[contentLen-1] == '\n' {
				content = content[:contentLen-1]
			}
		}

		switch pipe {
		case "stdout":
			stdout = append(stdout, content...)
		case "stderr":
			stderr = append(stderr, content...)
		default:
			// Complain about unknown pipes.
			logrus.Warnf("hit invalid log format [unknown pipe %s]: %q", pipe, string(line))
			continue
		}
	}

	return stdout, stderr
}

// ExecContainer prepares a streaming endpoint to execute a command in the container.
func (r *runtimeOCI) ExecContainer(c *Container, cmd []string, stdin io.Reader, stdout, stderr io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	processFile, err := prepareProcessExec(c, cmd, tty)
	if err != nil {
		return err
	}
	defer os.RemoveAll(processFile.Name())

	args := []string{rootFlag, r.root, "exec"}
	args = append(args, "--process", processFile.Name(), c.ID())
	execCmd := exec.Command(r.path, args...) // nolint: gosec
	if v, found := os.LookupEnv("XDG_RUNTIME_DIR"); found {
		execCmd.Env = append(execCmd.Env, fmt.Sprintf("XDG_RUNTIME_DIR=%s", v))
	}
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
func (r *runtimeOCI) ExecSyncContainer(c *Container, command []string, timeout int64) (*ExecSyncResponse, error) {
	pidFile, parentPipe, childPipe, err := prepareExec()
	if err != nil {
		return nil, &ExecSyncError{
			ExitCode: -1,
			Err:      err,
		}
	}
	defer parentPipe.Close()
	defer func() {
		if e := os.Remove(pidFile.Name()); e != nil {
			logrus.Warnf("could not remove temporary PID file %s", pidFile.Name())
		}
	}()

	logFile, err := ioutil.TempFile("", "crio-log-"+c.id)
	if err != nil {
		return nil, &ExecSyncError{
			ExitCode: -1,
			Err:      err,
		}
	}
	logPath := logFile.Name()
	defer func() {
		logFile.Close()
		os.RemoveAll(logPath)
	}()

	var args []string
	args = append(args,
		"-c", c.id,
		"-n", c.name,
		"-r", r.path,
		"-p", pidFile.Name(),
		"-e")
	if c.terminal {
		args = append(args, "-t")
	}
	if timeout > 0 {
		args = append(args, "-T", fmt.Sprintf("%d", timeout))
	}
	args = append(args,
		"-l", logPath,
		"--socket-dir-path", r.config.ContainerAttachSocketDir,
		"--log-level", logrus.GetLevel().String())

	processFile, err := prepareProcessExec(c, command, c.terminal)
	if err != nil {
		return nil, &ExecSyncError{
			ExitCode: -1,
			Err:      err,
		}
	}
	defer os.RemoveAll(processFile.Name())

	args = append(args,
		"--exec-process-spec", processFile.Name(),
		"--runtime-arg", fmt.Sprintf("%s=%s", rootFlag, r.root))

	cmd := exec.Command(r.config.Conmon, args...) // nolint: gosec

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	cmd.ExtraFiles = append(cmd.ExtraFiles, childPipe)
	// 0, 1 and 2 are stdin, stdout and stderr
	cmd.Env = r.config.ConmonEnv
	cmd.Env = append(cmd.Env, fmt.Sprintf("_OCI_SYNCPIPE=%d", 3))
	if v, found := os.LookupEnv("XDG_RUNTIME_DIR"); found {
		cmd.Env = append(cmd.Env, fmt.Sprintf("XDG_RUNTIME_DIR=%s", v))
	}

	err = cmd.Start()
	if err != nil {
		childPipe.Close()
		return nil, &ExecSyncError{
			Stdout:   stdoutBuf,
			Stderr:   stderrBuf,
			ExitCode: -1,
			Err:      err,
		}
	}

	// We don't need childPipe on the parent side
	childPipe.Close()

	err = cmd.Wait()
	if err != nil {
		return nil, &ExecSyncError{
			Stdout:   stdoutBuf,
			Stderr:   stderrBuf,
			ExitCode: getExitCode(err),
			Err:      err,
		}
	}

	var ec *exitCodeInfo
	if err := json.NewDecoder(parentPipe).Decode(&ec); err != nil {
		return nil, &ExecSyncError{
			Stdout:   stdoutBuf,
			Stderr:   stderrBuf,
			ExitCode: -1,
			Err:      err,
		}
	}

	logrus.Debugf("Received container exit code: %v, message: %s", ec.ExitCode, ec.Message)

	// When we timeout the command in conmon then we should return
	// an ExecSyncResponse with a non-zero exit code because
	// the prober code in the kubelet checks for it. If we return
	// a custom error, then the probes transition into Unknown status
	// and the container isn't restarted as expected.
	if ec.ExitCode == -1 && ec.Message == conmonconfig.TimedOutMessage {
		return &ExecSyncResponse{
			Stderr:   []byte(conmonconfig.TimedOutMessage),
			ExitCode: -1,
		}, nil
	}

	if ec.ExitCode == -1 {
		return nil, &ExecSyncError{
			Stdout:   stdoutBuf,
			Stderr:   stderrBuf,
			ExitCode: -1,
			Err:      fmt.Errorf(ec.Message),
		}
	}

	// The actual logged output is not the same as stdoutBuf and stderrBuf,
	// which are used for getting error information. For the actual
	// ExecSyncResponse we have to read the logfile.
	// XXX: Currently runC dups the same console over both stdout and stderr,
	//      so we can't differentiate between the two.

	logBytes, err := ioutil.ReadFile(logPath)
	if err != nil {
		return nil, &ExecSyncError{
			Stdout:   stdoutBuf,
			Stderr:   stderrBuf,
			ExitCode: -1,
			Err:      err,
		}
	}

	// We have to parse the log output into {stdout, stderr} buffers.
	stdoutBytes, stderrBytes := parseLog(logBytes)
	return &ExecSyncResponse{
		Stdout:   stdoutBytes,
		Stderr:   stderrBytes,
		ExitCode: ec.ExitCode,
	}, nil
}

// UpdateContainer updates container resources
func (r *runtimeOCI) UpdateContainer(c *Container, res *rspec.LinuxResources) error {
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

func waitContainerStop(ctx context.Context, c *Container, timeout time.Duration, ignoreKill bool) error {
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
				process, err := findprocess.FindProcess(c.state.Pid)
				if err != nil {
					if err != findprocess.ErrNotFound {
						logrus.Warnf("failed to find process %d for container %v: %v", c.state.Pid, c.id, err)
					}
					close(done)
					return
				}
				err = process.Release()
				if err != nil {
					logrus.Warnf("failed to release process %d for container %v: %v", c.state.Pid, c.id, err)
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		close(chControl)
		return ctx.Err()
	case <-time.After(timeout):
		close(chControl)
		if ignoreKill {
			return fmt.Errorf("timeout reached after %.0f seconds waiting for container process to exit",
				timeout.Seconds())
		}
		err := kill(c.state.Pid)
		if err != nil {
			return fmt.Errorf("failed to kill process: %v", err)
		}
	}

	c.state.Finished = time.Now()
	return nil
}

// StopContainer stops a container. Timeout is given in seconds.
func (r *runtimeOCI) StopContainer(ctx context.Context, c *Container, timeout int64) error {
	c.opLock.Lock()
	defer c.opLock.Unlock()

	if err := c.ShouldBeStopped(); err != nil {
		return err
	}

	// Check if the process is around before sending a signal
	process, err := findprocess.FindProcess(c.state.Pid)
	if err == findprocess.ErrNotFound {
		c.state.Finished = time.Now()
		return nil
	}
	if err != nil {
		logrus.Warnf("failed to find process %d for container %q: %v", c.state.Pid, c.id, err)
	} else {
		err = process.Release()
		if err != nil {
			logrus.Warnf("failed to release process %d for container %q: %v", c.state.Pid, c.id, err)
		}
	}

	if timeout > 0 {
		if _, err := utils.ExecCmd(
			r.path, rootFlag, r.root, "kill", c.id, c.GetStopSignal(),
		); err != nil {
			if err := checkProcessGone(c); err != nil {
				return fmt.Errorf("failed to stop container %q: %v", c.id, err)
			}
		}
		err = waitContainerStop(ctx, c, time.Duration(timeout)*time.Second, true)
		if err == nil {
			return nil
		}
		logrus.Warnf("Stopping container %v with stop signal timed out: %v", c.id, err)
	}

	if _, err := utils.ExecCmd(
		r.path, rootFlag, r.root, "kill", c.id, "KILL",
	); err != nil {
		if err := checkProcessGone(c); err != nil {
			return fmt.Errorf("failed to stop container %v: %v", c.id, err)
		}
	}

	return waitContainerStop(ctx, c, killContainerTimeout, false)
}

func checkProcessGone(c *Container) error {
	process, perr := findprocess.FindProcess(c.state.Pid)
	if perr == findprocess.ErrNotFound {
		c.state.Finished = time.Now()
		return nil
	}
	if perr == nil {
		err := process.Release()
		if err != nil {
			logrus.Warnf("failed to release process %d for container %v: %v", c.state.Pid, c.id, err)
		}
	}
	return fmt.Errorf("failed to find process: %v", perr)
}

// DeleteContainer deletes a container.
func (r *runtimeOCI) DeleteContainer(c *Container) error {
	c.opLock.Lock()
	defer c.opLock.Unlock()

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
func (r *runtimeOCI) UpdateContainerStatus(c *Container) error {
	c.opLock.Lock()
	defer c.opLock.Unlock()

	if c.state.ExitCode != nil && !c.state.Finished.IsZero() {
		logrus.Debugf("Skipping status update for: %+v", c.state)
		return nil
	}

	stateCmd := func() (*ContainerState, bool, error) {
		cmd := exec.Command(r.path, rootFlag, r.root, "state", c.id) // nolint: gosec
		if v, found := os.LookupEnv("XDG_RUNTIME_DIR"); found {
			cmd.Env = append(cmd.Env, fmt.Sprintf("XDG_RUNTIME_DIR=%s", v))
		}
		out, err := cmd.Output()
		if err != nil {
			logrus.Errorf("Failed to update container state for %s: %v", c.id, err)
			// there are many code paths that could lead to have a bad state in the
			// underlying runtime.
			// On any error like a container went away or we rebooted and containers
			// went away we do not error out stopping kubernetes to recover.
			// We always populate the fields below so kube can restart/reschedule
			// containers failing.
			if exitErr, isExitError := err.(*exec.ExitError); isExitError {
				logrus.Errorf("failed to update container state for %s: stdout: %s, stderr: %s", c.id, string(out), string(exitErr.Stderr))
			} else {
				logrus.Errorf("failed to update container state for %s: %v", c.id, err)
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
		logrus.Warnf("failed to find container exit file for %v: %v", c.id, err)
	} else {
		c.state.Finished, err = getFinishedTime(fi)
		if err != nil {
			return fmt.Errorf("failed to get finished time: %v", err)
		}
		statusCodeStr, err := ioutil.ReadFile(exitFilePath)
		if err != nil {
			return fmt.Errorf("failed to read exit file: %v", err)
		}
		statusCode, err := strconv.Atoi(string(statusCodeStr))
		if err != nil {
			return fmt.Errorf("status code conversion failed: %v", err)
		}
		c.state.ExitCode = utils.Int32Ptr(int32(statusCode))
		logrus.Debugf("found exit code for %s: %d", c.id, statusCode)
	}

	oomFilePath := filepath.Join(c.bundlePath, "oom")
	if _, err = os.Stat(oomFilePath); err == nil {
		c.state.OOMKilled = true
	}

	return nil
}

// PauseContainer pauses a container.
func (r *runtimeOCI) PauseContainer(c *Container) error {
	c.opLock.Lock()
	defer c.opLock.Unlock()

	_, err := utils.ExecCmd(r.path, rootFlag, r.root, "pause", c.id)
	return err
}

// UnpauseContainer unpauses a container.
func (r *runtimeOCI) UnpauseContainer(c *Container) error {
	c.opLock.Lock()
	defer c.opLock.Unlock()

	_, err := utils.ExecCmd(r.path, rootFlag, r.root, "resume", c.id)
	return err
}

func (r *runtimeOCI) WaitContainerStateStopped(ctx context.Context, c *Container) error {
	return nil
}

// ContainerStats provides statistics of a container.
func (r *runtimeOCI) ContainerStats(c *Container, cgroup string) (*ContainerStats, error) {
	c.opLock.Lock()
	defer c.opLock.Unlock()

	return r.containerStats(c, cgroup)
}

// SignalContainer sends a signal to a container process.
func (r *runtimeOCI) SignalContainer(c *Container, sig syscall.Signal) error {
	c.opLock.Lock()
	defer c.opLock.Unlock()

	if unix.SignalName(sig) == "" {
		return errors.Errorf("unable to find signal %s", sig.String())
	}

	_, err := utils.ExecCmd(
		r.path, rootFlag, r.root, "kill", c.ID(), strconv.Itoa(int(sig)),
	)
	return err
}

// AttachContainer attaches IO to a running container.
func (r *runtimeOCI) AttachContainer(c *Container, inputStream io.Reader, outputStream, errorStream io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	controlPath := filepath.Join(c.BundlePath(), "ctl")
	controlFile, err := os.OpenFile(controlPath, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("failed to open container ctl file: %v", err)
	}
	defer controlFile.Close()

	kubecontainer.HandleResizing(resize, func(size remotecommand.TerminalSize) {
		logrus.Debugf("Got a resize event: %+v", size)
		_, err := fmt.Fprintf(controlFile, "%d %d %d\n", 1, size.Height, size.Width)
		if err != nil {
			logrus.Debugf("Failed to write to control file to resize terminal: %v", err)
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

		// TODO: hardcoded to tcp4 because localhost resolves to ::1 by default
		// if the system has IPv6 enabled. However, not all applications are
		// listening on the IPv6 localhost address. Theoretically happy
		// eyeballs will try IPv6 first and fallback to IPv4 but resolving
		// localhost doesn't seem to return and IPv4 address always, thus
		// failing the connection.
		conn, err := net.Dial("tcp4", fmt.Sprintf("localhost:%d", port))
		if err != nil {
			return errors.Wrapf(err, "dialing %d", port)
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
func (r *runtimeOCI) ReopenContainerLog(c *Container) error {
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
				logrus.Debugf("event: %v", event)
				if event.Op&fsnotify.Create == fsnotify.Create || event.Op&fsnotify.Write == fsnotify.Write {
					logrus.Debugf("file created %s", event.Name)
					if event.Name == c.LogPath() {
						logrus.Debugf("expected log file created")
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
		logrus.Errorf("watcher.Add(%q) failed: %s", cLogDir, err)
		close(done)
		doneClosed = true
	}

	if _, err = fmt.Fprintf(controlFile, "%d %d %d\n", 2, 0, 0); err != nil {
		logrus.Debugf("Failed to write to control file to reopen log file: %v", err)
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
// caller is responsible to close the returned *os.File if needed.
func prepareProcessExec(c *Container, cmd []string, tty bool) (*os.File, error) {
	f, err := ioutil.TempFile("", "exec-process-")
	if err != nil {
		return nil, err
	}

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
		return nil, err
	}

	if err := ioutil.WriteFile(f.Name(), processJSON, 0o644); err != nil {
		return nil, err
	}
	return f, nil
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

// SpoofOOM is a function that sets a container state as though it OOM'd. It's used in situations
// where another process in the container's cgroup (like conmon) OOM'd when it wasn't supposed to,
// allowing us to report to the kubelet that the container OOM'd instead.
func (r *Runtime) SpoofOOM(c *Container) {
	ecBytes := []byte{'1', '3', '7'}

	c.opLock.Lock()
	defer c.opLock.Unlock()

	c.state.Status = ContainerStateStopped
	c.state.Finished = time.Now()
	c.state.ExitCode = utils.Int32Ptr(137)
	c.state.OOMKilled = true

	oomFilePath := filepath.Join(c.bundlePath, "oom")
	oomFile, err := os.Create(oomFilePath)
	if err != nil {
		logrus.Debugf("unable to write to oom file path %s: %v", oomFilePath, err)
	}
	oomFile.Close()

	exitFilePath := filepath.Join(r.config.ContainerExitsDir, c.id)
	exitFile, err := os.Create(exitFilePath)
	if err != nil {
		logrus.Debugf("unable to write exit file path %s: %v", exitFilePath, err)
		return
	}
	if _, err := exitFile.Write(ecBytes); err != nil {
		logrus.Debugf("failed to write exit code to file %s: %v", exitFilePath, err)
	}
	exitFile.Close()
}

func ConmonPath(r *Runtime) string {
	return r.config.Conmon
}
