package utils

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/containers/podman/v3/pkg/lookup"
	"github.com/cri-o/cri-o/internal/dbusmgr"
	"github.com/cri-o/cri-o/server/cri/types"
	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/opencontainers/runc/libcontainer/user"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context"

	systemdDbus "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/trace"
)

// ExecCmd executes a command with args and returns its output as a string along
// with an error, if any
func ExecCmd(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if v, found := os.LookupEnv("XDG_RUNTIME_DIR"); found {
		cmd.Env = append(cmd.Env, fmt.Sprintf("XDG_RUNTIME_DIR=%s", v))
	}

	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("`%v %v` failed: %v %v (%v)", name, strings.Join(args, " "), stderr.String(), stdout.String(), err)
	}

	return stdout.String(), nil
}

// StatusToExitCode converts wait status code to an exit code
func StatusToExitCode(status int) int {
	return ((status) & 0xff00) >> 8
}

// RunUnderSystemdScope adds the specified pid to a systemd scope
func RunUnderSystemdScope(mgr *dbusmgr.DbusConnManager, pid int, slice, unitName string, properties ...systemdDbus.Property) error {
	ctx := context.Background()
	// TODO: Testing tracer with Fn outside kubelet
	ctx = baggage.ContextWithValues(ctx,
		attribute.String("PID and unit name", fmt.Sprintf("%s %s", string(pid), unitName)),
	)
	tracer := otel.GetTracerProvider().Tracer("run-under-systemd-scope")
	var span trace.Span
	ctx, span = tracer.Start(ctx, "pid-to-systemd-scope")
	defer span.End()

	var err error
	// sanity check
	if mgr == nil {
		return errors.New("dbus manager is nil")
	}
	defaultProperties := []systemdDbus.Property{
		newProp("PIDs", []uint32{uint32(pid)}),
		newProp("Delegate", true),
		newProp("DefaultDependencies", false),
	}
	properties = append(defaultProperties, properties...)
	if slice != "" {
		properties = append(properties, systemdDbus.PropSlice(slice))
	}
	ch := make(chan string)
	if err := mgr.RetryOnDisconnect(func(c *systemdDbus.Conn) error {
		_, err = c.StartTransientUnitContext(ctx, unitName, "replace", properties, ch)
		return err
	}); err != nil {
		return err
	}
	span.AddEvent("started transient unit context", trace.WithAttributes(attribute.String(unitName, "replace")))

	// Block until job is started
	<-ch
	close(ch)

	return nil
}

func newProp(name string, units interface{}) systemdDbus.Property {
	return systemdDbus.Property{
		Name:  name,
		Value: dbus.MakeVariant(units),
	}
}

// DetachError is special error which returned in case of container detach.
type DetachError struct{}

func (DetachError) Error() string {
	return "detached from container"
}

// CopyDetachable is similar to io.Copy but support a detach key sequence to break out.
func CopyDetachable(dst io.Writer, src io.Reader, keys []byte) (int64, error) {
	var (
		written int64
		err     error
	)
	// Sanity check interfaces
	if dst == nil || src == nil {
		return 0, fmt.Errorf("src/dst reader/writer nil")
	}
	if len(keys) == 0 {
		// Default keys : ctrl-p ctrl-q
		keys = []byte{16, 17}
	}

	buf := make([]byte, 32*1024)
	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			preservBuf := []byte{}
			for i, key := range keys {
				preservBuf = append(preservBuf, buf[0:nr]...)
				if nr != 1 || buf[0] != key {
					break
				}
				if i == len(keys)-1 {
					// src.Close()
					return 0, DetachError{}
				}
				nr, er = src.Read(buf)
			}
			nw, ew := dst.Write(preservBuf)
			nr = len(preservBuf)
			if nw > 0 {
				written += int64(nw)
			}
			if ew != nil {
				err = ew
				break
			}
			if nr != nw {
				err = io.ErrShortWrite
				break
			}
		}
		if er != nil {
			if er != io.EOF {
				err = er
			}
			break
		}
	}
	return written, err
}

// WriteGoroutineStacks writes out the goroutine stacks
// of the caller. Up to 32 MB is allocated to print the
// stack.
func WriteGoroutineStacks(w io.Writer) error {
	if w == nil {
		return fmt.Errorf("writer nil")
	}
	buf := make([]byte, 1<<20)
	for i := 0; ; i++ {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		if len(buf) >= 32<<20 {
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	_, err := w.Write(buf)
	return err
}

// WriteGoroutineStacksToFile write goroutine stacks
// to the specified file.
func WriteGoroutineStacksToFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	defer f.Close()

	err = WriteGoroutineStacks(f)
	if err != nil {
		return err
	}
	return f.Sync()
}

// GenerateID generates a random unique id.
func GenerateID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", errors.Wrap(err, "generate ID")
	}
	return hex.EncodeToString(b), nil
}

// openContainerFile opens a file inside a container rootfs safely
func openContainerFile(rootfs, path string) (io.ReadCloser, error) {
	fp, err := securejoin.SecureJoin(rootfs, path)
	if err != nil {
		return nil, err
	}
	fh, err := os.Open(fp)
	if err != nil {
		// This is needed because a nil *os.File is different to a nil
		// io.ReadCloser and this causes GetExecUser to not detect that the
		// container file is missing.
		return nil, err
	}
	return fh, nil
}

// GetUserInfo returns UID, GID and additional groups for specified user
// by looking them up in /etc/passwd and /etc/group
func GetUserInfo(rootfs, userName string) (uid, gid uint32, additionalGids []uint32, _ error) {
	// We don't care if we can't open the file because
	// not all images will have these files
	passwdFile, err := openContainerFile(rootfs, "/etc/passwd")
	if err != nil {
		logrus.Warnf("Failed to open /etc/passwd: %v", err)
	} else {
		defer passwdFile.Close()
	}

	groupFile, err := openContainerFile(rootfs, "/etc/group")
	if err != nil {
		logrus.Warnf("Failed to open /etc/group: %v", err)
	} else {
		defer groupFile.Close()
	}

	execUser, err := user.GetExecUser(userName, nil, passwdFile, groupFile)
	if err != nil {
		return 0, 0, nil, errors.Wrap(err, "get exec user")
	}

	uid = uint32(execUser.Uid)
	gid = uint32(execUser.Gid)
	additionalGids = make([]uint32, 0, len(execUser.Sgids))
	for _, g := range execUser.Sgids {
		additionalGids = append(additionalGids, uint32(g))
	}

	return uid, gid, additionalGids, nil
}

// GeneratePasswd generates a container specific passwd file,
// iff uid is not defined in the containers /etc/passwd
func GeneratePasswd(username string, uid, gid uint32, homedir, rootfs, rundir string) (string, error) {
	// if UID exists inside of container rootfs /etc/passwd then
	// don't generate passwd
	if _, err := lookup.GetUser(rootfs, strconv.Itoa(int(uid))); err == nil {
		return "", nil
	}
	passwdFile := filepath.Join(rundir, "passwd")
	originPasswdFile, err := securejoin.SecureJoin(rootfs, "/etc/passwd")
	if err != nil {
		return "", errors.Wrap(err, "unable to follow symlinks to passwd file")
	}
	info, err := os.Stat(originPasswdFile)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", errors.Wrapf(err, "unable to stat passwd file %s", originPasswdFile)
	}
	// Check if passwd file is world writable
	if info.Mode().Perm()&(0o022) != 0 {
		return "", nil
	}
	passwdUID := info.Sys().(*syscall.Stat_t).Uid
	passwdGID := info.Sys().(*syscall.Stat_t).Gid

	if uid == passwdUID && info.Mode().Perm()&(0o200) != 0 {
		return "", nil
	}

	orig, err := ioutil.ReadFile(originPasswdFile)
	if err != nil {
		// If no /etc/passwd in container ignore and return
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", errors.Wrapf(err, "read passwd file")
	}
	if username == "" {
		username = "default"
	}
	if homedir == "" {
		homedir = "/tmp"
	}
	pwd := fmt.Sprintf("%s%s:x:%d:%d:%s user:%s:/sbin/nologin\n", orig, username, uid, gid, username, homedir)
	if err := ioutil.WriteFile(passwdFile, []byte(pwd), info.Mode()); err != nil {
		return "", errors.Wrap(err, "failed to create temporary passwd file")
	}
	if err := os.Chown(passwdFile, int(passwdUID), int(passwdGID)); err != nil {
		return "", errors.Wrap(err, "failed to chown temporary passwd file")
	}

	return passwdFile, nil
}

// Int32Ptr is a utility function to assign to integer pointer variables
func Int32Ptr(i int32) *int32 {
	return &i
}

// EnsureSaneLogPath is a hack to fix https://issues.k8s.io/44043 which causes
// logPath to be a broken symlink to some magical Docker path. Ideally we
// wouldn't have to deal with this, but until that issue is fixed we have to
// remove the path if it's a broken symlink.
func EnsureSaneLogPath(logPath string) error {
	// If the path exists but the resolved path does not, then we have a broken
	// symlink and we need to remove it.
	fi, err := os.Lstat(logPath)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		// Non-existent files and non-symlinks aren't our problem.
		return nil
	}

	_, err = os.Stat(logPath)
	if os.IsNotExist(err) {
		err = os.RemoveAll(logPath)
		if err != nil {
			return fmt.Errorf("failed to remove bad log path %s: %v", logPath, err)
		}
	}
	return nil
}

func GetLabelOptions(selinuxOptions *types.SELinuxOption) []string {
	labels := []string{}
	if selinuxOptions != nil {
		if selinuxOptions.User != "" {
			labels = append(labels, "user:"+selinuxOptions.User)
		}
		if selinuxOptions.Role != "" {
			labels = append(labels, "role:"+selinuxOptions.Role)
		}
		if selinuxOptions.Type != "" {
			labels = append(labels, "type:"+selinuxOptions.Type)
		}
		if selinuxOptions.Level != "" {
			labels = append(labels, "level:"+selinuxOptions.Level)
		}
	}
	return labels
}

// SyncParent ensures a path's parent directory is synced to disk
func SyncParent(path string) error {
	return Sync(filepath.Dir(path))
}

// Sync ensures a path is synced to disk
func Sync(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY, 0o755)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := syscall.Fsync(int(f.Fd())); err != nil {
		return err
	}
	return nil
}
