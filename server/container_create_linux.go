// +build linux

package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/containers/libpod/pkg/apparmor"
	"github.com/containers/libpod/pkg/secrets"
	createconfig "github.com/containers/libpod/pkg/spec"
	"github.com/containers/storage/pkg/idtools"
	dockermounts "github.com/docker/docker/pkg/mount"
	"github.com/docker/docker/pkg/symlink"
	"github.com/kubernetes-sigs/cri-o/lib/sandbox"
	"github.com/kubernetes-sigs/cri-o/oci"
	"github.com/kubernetes-sigs/cri-o/pkg/annotations"
	"github.com/kubernetes-sigs/cri-o/pkg/storage"
	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/opencontainers/runc/libcontainer/devices"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
	pb "k8s.io/kubernetes/pkg/kubelet/apis/cri/runtime/v1alpha2"
)

// minMemoryLimit is the minimum memory that must be set for a container.
// A lower value would result in the container failing to start.
const minMemoryLimit = 4194304

func findCgroupMountpoint(name string) error {
	// Set up pids limit if pids cgroup is mounted
	_, err := cgroups.FindCgroupMountpoint(name)
	return err
}

func addDevicesPlatform(sb *sandbox.Sandbox, containerConfig *pb.ContainerConfig, specgen *generate.Generator) error {
	sp := specgen.Spec()
	if containerConfig.GetLinux().GetSecurityContext().GetPrivileged() {
		hostDevices, err := devices.HostDevices()
		if err != nil {
			return err
		}
		for _, hostDevice := range hostDevices {
			rd := rspec.LinuxDevice{
				Path:  hostDevice.Path,
				Type:  string(hostDevice.Type),
				Major: hostDevice.Major,
				Minor: hostDevice.Minor,
				UID:   &hostDevice.Uid,
				GID:   &hostDevice.Gid,
			}
			if hostDevice.Major == 0 && hostDevice.Minor == 0 {
				// Invalid device, most likely a symbolic link, skip it.
				continue
			}
			specgen.AddDevice(rd)
		}
		sp.Linux.Resources.Devices = []rspec.LinuxDeviceCgroup{
			{
				Allow:  true,
				Access: "rwm",
			},
		}
	}

	for _, device := range containerConfig.GetDevices() {
		// If we are privileged, we have access to devices on the host.
		// If the requested container path already exists on the host, the container won't see the expected host path.
		// Therefore, we must error out if the container path already exists
		privileged := containerConfig.GetLinux().GetSecurityContext() != nil && containerConfig.GetLinux().GetSecurityContext().GetPrivileged()
		if privileged && device.ContainerPath != device.HostPath {
			// we expect this to not exist
			_, err := os.Stat(device.ContainerPath)
			if err == nil {
				return errors.Errorf("privileged container was configured with a device container path that already exists on the host.")
			}
			if !os.IsNotExist(err) {
				return errors.Wrapf(err, "error checking if container path exists on host")
			}
		}

		path, err := resolveSymbolicLink(device.HostPath, "/")
		if err != nil {
			return err
		}
		dev, err := devices.DeviceFromPath(path, device.Permissions)
		// if there was no error, return the device
		if err == nil {
			rd := rspec.LinuxDevice{
				Path:  device.ContainerPath,
				Type:  string(dev.Type),
				Major: dev.Major,
				Minor: dev.Minor,
				UID:   &dev.Uid,
				GID:   &dev.Gid,
			}
			specgen.AddDevice(rd)
			sp.Linux.Resources.Devices = append(sp.Linux.Resources.Devices, rspec.LinuxDeviceCgroup{
				Allow:  true,
				Type:   string(dev.Type),
				Major:  &dev.Major,
				Minor:  &dev.Minor,
				Access: dev.Permissions,
			})
			continue
		}
		// if the device is not a device node
		// try to see if it's a directory holding many devices
		if err == devices.ErrNotADevice {

			// check if it is a directory
			if src, e := os.Stat(path); e == nil && src.IsDir() {

				// mount the internal devices recursively
				filepath.Walk(path, func(dpath string, f os.FileInfo, e error) error {
					if e != nil {
						logrus.Debugf("addDevice walk: %v", e)
					}
					childDevice, e := devices.DeviceFromPath(dpath, device.Permissions)
					if e != nil {
						// ignore the device
						return nil
					}
					cPath := strings.Replace(dpath, path, device.ContainerPath, 1)
					rd := rspec.LinuxDevice{
						Path:  cPath,
						Type:  string(childDevice.Type),
						Major: childDevice.Major,
						Minor: childDevice.Minor,
						UID:   &childDevice.Uid,
						GID:   &childDevice.Gid,
					}
					specgen.AddDevice(rd)
					sp.Linux.Resources.Devices = append(sp.Linux.Resources.Devices, rspec.LinuxDeviceCgroup{
						Allow:  true,
						Type:   string(childDevice.Type),
						Major:  &childDevice.Major,
						Minor:  &childDevice.Minor,
						Access: childDevice.Permissions,
					})

					return nil
				})
			}
		}
	}
	return nil
}

// createContainerPlatform performs platform dependent intermediate steps before calling the container's oci.Runtime().CreateContainer()
func (s *Server) createContainerPlatform(container *oci.Container, infraContainer *oci.Container, cgroupParent string) error {
	intermediateMountPoint := container.IntermediateMountPoint()

	if intermediateMountPoint == "" {
		return s.Runtime().CreateContainer(container, cgroupParent)
	}

	errc := make(chan error)
	go func() {
		// We create a new mount namespace before running the container as the rootfs of the
		// container is accessible only to the root user.  We use the intermediate mount
		// namespace to bind mount the root to a directory that is accessible to the user which
		// maps to root inside of the container/
		// We carefully unlock the OS thread only if no errors happened.  The thread might have failed
		// to restore the original mount namespace, and unlocking it will let it keep running
		// in a different context than the other threads.  A thread that is still locked when the
		// goroutine terminates is automatically destroyed.
		var err error
		runtime.LockOSThread()
		defer func() {
			if err == nil {
				runtime.UnlockOSThread()
			}
			errc <- err
		}()

		fd, err := os.Open(fmt.Sprintf("/proc/%d/task/%d/ns/mnt", os.Getpid(), unix.Gettid()))
		if err != nil {
			return
		}
		defer fd.Close()

		// create a new mountns on the current thread
		if err = unix.Unshare(unix.CLONE_NEWNS); err != nil {
			return
		}
		defer unix.Setns(int(fd.Fd()), unix.CLONE_NEWNS)

		// don't spread our mounts around
		err = unix.Mount("/", "/", "none", unix.MS_REC|unix.MS_SLAVE, "")
		if err != nil {
			return
		}

		rootUID, rootGID, err := idtools.GetRootUIDGID(container.IDMappings().UIDs(), container.IDMappings().GIDs())
		if err != nil {
			return
		}

		err = os.Chown(intermediateMountPoint, rootUID, rootGID)
		if err != nil {
			return
		}
		logrus.Debugf("intermediateMountPoint %s, rootUID %v, rootGID %v", intermediateMountPoint, rootUID, rootGID)
		mountPoint := container.MountPoint()
		err = os.Chown(mountPoint, rootUID, rootGID)
		if err != nil {
			return
		}

		rootPath := filepath.Join(intermediateMountPoint, "root")
		err = idtools.MkdirAllAs(rootPath, 0700, rootUID, rootGID)
		if err != nil {
			return
		}

		err = unix.Mount(mountPoint, rootPath, "none", unix.MS_BIND, "")
		if err != nil {
			return
		}

		if infraContainer != nil {
			infraRunDir := filepath.Join(intermediateMountPoint, "infra-rundir")
			err = idtools.MkdirAllAs(infraRunDir, 0700, rootUID, rootGID)
			if err != nil {
				return
			}

			err = unix.Mount(infraContainer.BundlePath(), infraRunDir, "none", unix.MS_BIND, "")
			if err != nil {
				return
			}
			err = os.Chown(infraRunDir, rootUID, rootGID)
			if err != nil {
				return
			}
		}

		runDirPath := filepath.Join(intermediateMountPoint, "rundir")
		err = idtools.MkdirAllAs(runDirPath, 0700, rootUID, rootGID)
		if err != nil {
			return
		}
		err = chownAllFilesAt(container.BundlePath(), rootUID, rootGID)
		if err != nil {
			logrus.Errorf("err in chowning container.BundlePath() with rootUID %v rootGID %v: %v", rootUID, rootGID, err)
			return
		}
		logrus.Debugf("chowned container.BundlePath(), %s,  with rootUID %v rootGID %v", container.BundlePath(), rootUID, rootGID)
		err = unix.Mount(container.BundlePath(), runDirPath, "none", unix.MS_BIND, "suid")
		if err != nil {
			return
		}

		err = s.Runtime().CreateContainer(container, cgroupParent)
	}()

	err := <-errc
	return err
}

func chownAllFilesAt(dir string, uid int, gid int) error {
	var files []string

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		files = append(files, path)
		return nil
	})
	if err != nil {
		return err
	}
	for _, file := range files {
		err = os.Chown(file, uid, gid)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) createSandboxContainer(ctx context.Context, containerID string, containerName string, sb *sandbox.Sandbox, sandboxConfig *pb.PodSandboxConfig, containerConfig *pb.ContainerConfig) (*oci.Container, error) {
	if sb == nil {
		return nil, errors.New("createSandboxContainer needs a sandbox")
	}

	// TODO: simplify this function (cyclomatic complexity here is high)
	// TODO: factor generating/updating the spec into something other projects can vendor

	// creates a spec Generator with the default spec.
	specgen, err := generate.New("linux")
	if err != nil {
		return nil, err
	}
	specgen.HostSpecific = true
	specgen.ClearProcessRlimits()

	ulimits, err := getUlimitsFromConfig(s.config)
	if err != nil {
		return nil, err
	}
	for _, u := range ulimits {
		specgen.AddProcessRlimits(u.name, u.hard, u.soft)
	}

	readOnlyRootfs := s.config.ReadOnly

	var privileged bool
	if containerConfig.GetLinux().GetSecurityContext() != nil {
		if containerConfig.GetLinux().GetSecurityContext().GetPrivileged() {
			privileged = true
		}
		if privileged {
			if !sandboxConfig.GetLinux().GetSecurityContext().GetPrivileged() {
				return nil, errors.New("no privileged container allowed in sandbox")
			}
		}
		if containerConfig.GetLinux().GetSecurityContext().GetReadonlyRootfs() {
			readOnlyRootfs = true
		}
	}
	specgen.SetRootReadonly(readOnlyRootfs)

	if s.config.ReadOnly {
		// tmpcopyup is a runc extension and is not part of the OCI spec.
		// WORK ON: Use "overlay" mounts as an alternative to tmpfs with tmpcopyup
		// Look at https://github.com/kubernetes-sigs/cri-o/pull/1434#discussion_r177200245 for more info on this
		options := []string{"rw", "noexec", "nosuid", "nodev", "tmpcopyup"}
		if !isInCRIMounts("/run", containerConfig.GetMounts()) {
			mnt := rspec.Mount{
				Destination: "/run",
				Type:        "tmpfs",
				Source:      "tmpfs",
				Options:     append(options, "mode=0755"),
			}
			// Add tmpfs mount on /run
			specgen.AddMount(mnt)
		}
		if !isInCRIMounts("/tmp", containerConfig.GetMounts()) {
			mnt := rspec.Mount{
				Destination: "/tmp",
				Type:        "tmpfs",
				Source:      "tmpfs",
				Options:     append(options, "mode=1777"),
			}
			// Add tmpfs mount on /tmp
			specgen.AddMount(mnt)
		}
		if !isInCRIMounts("/var/tmp", containerConfig.GetMounts()) {
			mnt := rspec.Mount{
				Destination: "/var/tmp",
				Type:        "tmpfs",
				Source:      "tmpfs",
				Options:     append(options, "mode=1777"),
			}
			// Add tmpfs mount on /var/tmp
			specgen.AddMount(mnt)
		}
	}
	mountLabel := sb.MountLabel()
	processLabel := sb.ProcessLabel()
	selinuxConfig := containerConfig.GetLinux().GetSecurityContext().GetSelinuxOptions()
	if selinuxConfig != nil {
		var err error
		processLabel, mountLabel, err = getSELinuxLabels(selinuxConfig, privileged)
		if err != nil {
			return nil, err
		}
	}

	containerVolumes, ociMounts, err := addOCIBindMounts(mountLabel, containerConfig, &specgen, s.config.RuntimeConfig.BindMountPrefix)
	if err != nil {
		return nil, err
	}

	volumesJSON, err := json.Marshal(containerVolumes)
	if err != nil {
		return nil, err
	}
	specgen.AddAnnotation(annotations.Volumes, string(volumesJSON))

	mnt := rspec.Mount{
		Destination: "/sys/fs/cgroup",
		Type:        "cgroup",
		Source:      "cgroup",
		Options:     []string{"nosuid", "noexec", "nodev", "relatime", "ro"},
	}
	// Add cgroup mount so container process can introspect its own limits
	specgen.AddMount(mnt)

	devices, err := getDevicesFromConfig(s.config)
	if err != nil {
		return nil, err
	}
	for _, d := range devices {
		specgen.AddDevice(d)
	}

	if err := addDevices(sb, containerConfig, &specgen); err != nil {
		return nil, err
	}

	labels := containerConfig.GetLabels()

	if err := validateLabels(labels); err != nil {
		return nil, err
	}

	metadata := containerConfig.GetMetadata()

	kubeAnnotations := containerConfig.GetAnnotations()
	for k, v := range kubeAnnotations {
		specgen.AddAnnotation(k, v)
	}
	for k, v := range labels {
		specgen.AddAnnotation(k, v)
	}

	// set this container's apparmor profile if it is set by sandbox
	if s.appArmorEnabled && !privileged {

		appArmorProfileName := s.getAppArmorProfileName(containerConfig.GetLinux().GetSecurityContext().GetApparmorProfile())
		if appArmorProfileName != "" {
			// reload default apparmor profile if it is unloaded.
			if s.appArmorProfile == apparmorDefaultProfile {
				isLoaded, err := apparmor.IsLoaded(apparmorDefaultProfile)
				if err != nil {
					return nil, err
				}
				if !isLoaded {
					if err := apparmor.InstallDefault(apparmorDefaultProfile); err != nil {
						return nil, err
					}
				}
			}

			specgen.SetProcessApparmorProfile(appArmorProfileName)
		}

	}

	logPath := containerConfig.GetLogPath()
	sboxLogDir := sandboxConfig.GetLogDirectory()
	if sboxLogDir == "" {
		sboxLogDir = sb.LogDir()
	}
	if logPath == "" {
		logPath = filepath.Join(sboxLogDir, containerID+".log")
	}
	if !filepath.IsAbs(logPath) {
		// XXX: It's not really clear what this should be versus the sbox logDirectory.
		logrus.Warnf("requested logPath for ctr id %s is a relative path: %s", containerID, logPath)
		logPath = filepath.Join(sboxLogDir, logPath)
		logrus.Warnf("logPath from relative path is now absolute: %s", logPath)
	}

	// Handle https://issues.k8s.io/44043
	if err := ensureSaneLogPath(logPath); err != nil {
		return nil, err
	}

	logrus.WithFields(logrus.Fields{
		"sbox.logdir": sboxLogDir,
		"ctr.logfile": containerConfig.GetLogPath(),
		"log_path":    logPath,
	}).Debugf("setting container's log_path")

	specgen.SetProcessTerminal(containerConfig.Tty)
	if containerConfig.Tty {
		specgen.AddProcessEnv("TERM", "xterm")
	}

	linux := containerConfig.GetLinux()
	if linux != nil {
		resources := linux.GetResources()
		if resources != nil {
			specgen.SetLinuxResourcesCPUPeriod(uint64(resources.GetCpuPeriod()))
			specgen.SetLinuxResourcesCPUQuota(resources.GetCpuQuota())
			specgen.SetLinuxResourcesCPUShares(uint64(resources.GetCpuShares()))

			memoryLimit := resources.GetMemoryLimitInBytes()
			if memoryLimit != 0 && memoryLimit < minMemoryLimit {
				return nil, fmt.Errorf("set memory limit %v too low; should be at least %v", memoryLimit, minMemoryLimit)
			}
			specgen.SetLinuxResourcesMemoryLimit(memoryLimit)

			specgen.SetProcessOOMScoreAdj(int(resources.GetOomScoreAdj()))
			specgen.SetLinuxResourcesCPUCpus(resources.GetCpusetCpus())
			specgen.SetLinuxResourcesCPUMems(resources.GetCpusetMems())
		}

		var cgPath string
		parent := defaultCgroupfsParent
		useSystemd := s.config.CgroupManager == oci.SystemdCgroupsManager
		if useSystemd {
			parent = defaultSystemdParent
		}
		if sb.CgroupParent() != "" {
			parent = sb.CgroupParent()
		}
		if useSystemd {
			cgPath = parent + ":" + scopePrefix + ":" + containerID
		} else {
			cgPath = filepath.Join(parent, scopePrefix+"-"+containerID)
		}
		specgen.SetLinuxCgroupsPath(cgPath)

		if privileged {
			specgen.SetupPrivileged(true)
			setOCIBindMountsPrivileged(&specgen)
		} else {
			capabilities := linux.GetSecurityContext().GetCapabilities()
			// Ensure we don't get a nil pointer error if the config
			// doesn't set any capabilities
			if capabilities == nil {
				capabilities = &pb.Capability{}
			}
			// Clear default capabilities from spec
			specgen.ClearProcessCapabilities()
			for _, defaultCap := range s.config.DefaultCapabilities {
				capabilities.AddCapabilities = append(capabilities.AddCapabilities, defaultCap)
			}
			err = setupCapabilities(&specgen, capabilities)
			if err != nil {
				return nil, err
			}
		}
		specgen.SetProcessSelinuxLabel(processLabel)
		specgen.SetLinuxMountLabel(mountLabel)
		specgen.SetProcessNoNewPrivileges(linux.GetSecurityContext().GetNoNewPrivs())

		if containerConfig.GetLinux().GetSecurityContext() != nil &&
			!containerConfig.GetLinux().GetSecurityContext().Privileged {
			// TODO(runcom): have just one of this var at the top of the function
			securityContext := containerConfig.GetLinux().GetSecurityContext()
			for _, mp := range []string{
				"/proc/acpi",
				"/proc/kcore",
				"/proc/keys",
				"/proc/latency_stats",
				"/proc/timer_list",
				"/proc/timer_stats",
				"/proc/sched_debug",
				"/proc/scsi",
				"/sys/firmware",
			} {
				specgen.AddLinuxMaskedPaths(mp)
			}
			if securityContext.GetMaskedPaths() != nil {
				specgen.Spec().Linux.MaskedPaths = nil
				for _, path := range securityContext.GetMaskedPaths() {
					specgen.AddLinuxMaskedPaths(path)
				}
			}

			for _, rp := range []string{
				"/proc/asound",
				"/proc/bus",
				"/proc/fs",
				"/proc/irq",
				"/proc/sys",
				"/proc/sysrq-trigger",
			} {
				specgen.AddLinuxReadonlyPaths(rp)
			}
			if securityContext.GetReadonlyPaths() != nil {
				specgen.Spec().Linux.ReadonlyPaths = nil
				for _, path := range securityContext.GetReadonlyPaths() {
					specgen.AddLinuxReadonlyPaths(path)
				}
			}
		}
	}
	// Join the namespace paths for the pod sandbox container.
	podInfraState := sb.InfraContainer().State()

	logrus.Debugf("pod container state %+v", podInfraState)

	ipcNsPath := fmt.Sprintf("/proc/%d/ns/ipc", podInfraState.Pid)
	if err := specgen.AddOrReplaceLinuxNamespace(string(rspec.IPCNamespace), ipcNsPath); err != nil {
		return nil, err
	}

	utsNsPath := fmt.Sprintf("/proc/%d/ns/uts", podInfraState.Pid)
	if err := specgen.AddOrReplaceLinuxNamespace(string(rspec.UTSNamespace), utsNsPath); err != nil {
		return nil, err
	}

	if containerConfig.GetLinux().GetSecurityContext().GetNamespaceOptions().GetPid() == pb.NamespaceMode_NODE {
		// kubernetes PodSpec specify to use Host PID namespace
		specgen.RemoveLinuxNamespace(string(rspec.PIDNamespace))
	} else if containerConfig.GetLinux().GetSecurityContext().GetNamespaceOptions().GetPid() == pb.NamespaceMode_POD {
		// share Pod PID namespace
		pidNsPath := fmt.Sprintf("/proc/%d/ns/pid", podInfraState.Pid)
		if err := specgen.AddOrReplaceLinuxNamespace(string(rspec.PIDNamespace), pidNsPath); err != nil {
			return nil, err
		}
	}

	// If the sandbox is configured to run in the host network, do not create a new network namespace
	if sb.HostNetwork() {
		if err := specgen.RemoveLinuxNamespace(string(rspec.NetworkNamespace)); err != nil {
			return nil, err
		}
		specgen.RemoveMount("/sys")
		specgen.RemoveMount("/sys/cgroup")
		sysMnt := rspec.Mount{
			Destination: "/sys",
			Type:        "bind",
			Source:      "/sys",
			Options:     []string{"nosuid", "noexec", "nodev", "ro", "rbind"},
		}
		specgen.AddMount(sysMnt)
	} else {
		netNsPath := sb.NetNsPath()
		if netNsPath == "" {
			// The sandbox does not have a permanent namespace,
			// it's on the host one.
			netNsPath = fmt.Sprintf("/proc/%d/ns/net", podInfraState.Pid)
		}

		if err := specgen.AddOrReplaceLinuxNamespace(string(rspec.NetworkNamespace), netNsPath); err != nil {
			return nil, err
		}
	}

	imageSpec := containerConfig.GetImage()
	if imageSpec == nil {
		return nil, fmt.Errorf("CreateContainerRequest.ContainerConfig.Image is nil")
	}

	image := imageSpec.Image
	if image == "" {
		return nil, fmt.Errorf("CreateContainerRequest.ContainerConfig.Image.Image is empty")
	}
	images, err := s.StorageImageServer().ResolveNames(image)
	if err != nil {
		if err == storage.ErrCannotParseImageID {
			images = append(images, image)
		} else {
			return nil, err
		}
	}

	// Get imageName and imageRef that are later requested in container status
	var (
		imgResult    *storage.ImageResult
		imgResultErr error
	)
	for _, img := range images {
		imgResult, imgResultErr = s.StorageImageServer().ImageStatus(s.ImageContext(), img)
		if imgResultErr == nil {
			break
		}
	}
	if imgResultErr != nil {
		return nil, imgResultErr
	}

	imageName := imgResult.Name
	imageRef := imgResult.ID
	if len(imgResult.RepoDigests) > 0 {
		imageRef = imgResult.RepoDigests[0]
	}

	specgen.AddAnnotation(annotations.Image, image)
	specgen.AddAnnotation(annotations.ImageName, imageName)
	specgen.AddAnnotation(annotations.ImageRef, imageRef)
	specgen.AddAnnotation(annotations.IP, sb.IP())

	// Remove the default /dev/shm mount to ensure we overwrite it
	specgen.RemoveMount("/dev/shm")

	mnt = rspec.Mount{
		Type:        "bind",
		Source:      sb.ShmPath(),
		Destination: "/dev/shm",
		Options:     []string{"rw", "bind"},
	}
	// bind mount the pod shm
	specgen.AddMount(mnt)

	options := []string{"rw"}
	if readOnlyRootfs {
		options = []string{"ro"}
	}
	if sb.ResolvPath() != "" {
		if err := securityLabel(sb.ResolvPath(), mountLabel, false); err != nil {
			return nil, err
		}

		mnt = rspec.Mount{
			Type:        "bind",
			Source:      sb.ResolvPath(),
			Destination: "/etc/resolv.conf",
			Options:     []string{"bind", "nodev", "nosuid", "noexec"},
		}
		// bind mount the pod resolver file
		specgen.AddMount(mnt)
	}

	if sb.HostnamePath() != "" {
		if err := securityLabel(sb.HostnamePath(), mountLabel, false); err != nil {
			return nil, err
		}

		mnt = rspec.Mount{
			Type:        "bind",
			Source:      sb.HostnamePath(),
			Destination: "/etc/hostname",
			Options:     append(options, "bind"),
		}
		specgen.AddMount(mnt)
	}

	if !isInCRIMounts("/etc/hosts", containerConfig.GetMounts()) && hostNetwork(containerConfig) {
		// Only bind mount for host netns and when CRI does not give us any hosts file
		mnt = rspec.Mount{
			Type:        "bind",
			Source:      "/etc/hosts",
			Destination: "/etc/hosts",
			Options:     append(options, "bind"),
		}
		specgen.AddMount(mnt)
	}

	// Set hostname and add env for hostname
	specgen.SetHostname(sb.Hostname())
	specgen.AddProcessEnv("HOSTNAME", sb.Hostname())

	specgen.AddAnnotation(annotations.Name, containerName)
	specgen.AddAnnotation(annotations.ContainerID, containerID)
	specgen.AddAnnotation(annotations.SandboxID, sb.ID())
	specgen.AddAnnotation(annotations.SandboxName, sb.InfraContainer().Name())
	specgen.AddAnnotation(annotations.ContainerType, annotations.ContainerTypeContainer)
	specgen.AddAnnotation(annotations.LogPath, logPath)
	specgen.AddAnnotation(annotations.TTY, fmt.Sprintf("%v", containerConfig.Tty))
	specgen.AddAnnotation(annotations.Stdin, fmt.Sprintf("%v", containerConfig.Stdin))
	specgen.AddAnnotation(annotations.StdinOnce, fmt.Sprintf("%v", containerConfig.StdinOnce))
	specgen.AddAnnotation(annotations.ResolvPath, sb.InfraContainer().CrioAnnotations()[annotations.ResolvPath])

	created := time.Now()
	specgen.AddAnnotation(annotations.Created, created.Format(time.RFC3339Nano))

	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	specgen.AddAnnotation(annotations.Metadata, string(metadataJSON))

	labelsJSON, err := json.Marshal(labels)
	if err != nil {
		return nil, err
	}
	specgen.AddAnnotation(annotations.Labels, string(labelsJSON))

	kubeAnnotationsJSON, err := json.Marshal(kubeAnnotations)
	if err != nil {
		return nil, err
	}
	specgen.AddAnnotation(annotations.Annotations, string(kubeAnnotationsJSON))

	spp := containerConfig.GetLinux().GetSecurityContext().GetSeccompProfilePath()
	if !privileged {
		if err = s.setupSeccomp(&specgen, spp); err != nil {
			return nil, err
		}
	}
	specgen.AddAnnotation(annotations.SeccompProfilePath, spp)

	containerIDMappings := s.defaultIDMappings

	metaname := metadata.Name
	attempt := metadata.Attempt
	containerInfo, err := s.StorageRuntimeServer().CreateContainer(s.ImageContext(),
		sb.Name(), sb.ID(),
		image, imgResult.ID,
		containerName, containerID,
		metaname,
		attempt,
		mountLabel,
		containerIDMappings)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err2 := s.StorageRuntimeServer().DeleteContainer(containerInfo.ID)
			if err2 != nil {
				logrus.Warnf("Failed to cleanup container directory: %v", err2)
			}
		}
	}()

	mountPoint, err := s.StorageRuntimeServer().StartContainer(containerID)
	if err != nil {
		return nil, fmt.Errorf("failed to mount container %s(%s): %v", containerName, containerID, err)
	}
	specgen.AddAnnotation(annotations.MountPoint, mountPoint)

	containerImageConfig := containerInfo.Config
	if containerImageConfig == nil {
		err = fmt.Errorf("empty image config for %s", image)
		return nil, err
	}

	if containerImageConfig.Config.StopSignal != "" {
		// this key is defined in image-spec conversion document at https://github.com/opencontainers/image-spec/pull/492/files#diff-8aafbe2c3690162540381b8cdb157112R57
		specgen.AddAnnotation("org.opencontainers.image.stopSignal", containerImageConfig.Config.StopSignal)
	}

	// Add image volumes
	volumeMounts, err := addImageVolumes(mountPoint, s, &containerInfo, &specgen, mountLabel)
	if err != nil {
		return nil, err
	}

	processArgs := []string{}
	if containerImageConfig != nil {
		processArgs, err = buildOCIProcessArgs(containerConfig, &containerImageConfig.Config)
		if err != nil {
			return nil, err
		}
	}
	if len(processArgs) == 0 {
		return nil, fmt.Errorf("no command specified")
	}
	specgen.SetProcessArgs(processArgs)

	envs := mergeEnvs(containerImageConfig, containerConfig.GetEnvs())
	for _, e := range envs {
		parts := strings.SplitN(e, "=", 2)
		specgen.AddProcessEnv(parts[0], parts[1])
	}

	// Set working directory
	// Pick it up from image config first and override if specified in CRI
	containerCwd := "/"
	if containerImageConfig != nil {
		imageCwd := containerImageConfig.Config.WorkingDir
		if imageCwd != "" {
			containerCwd = imageCwd
		}
	}
	runtimeCwd := containerConfig.WorkingDir
	if runtimeCwd != "" {
		containerCwd = runtimeCwd
	}
	specgen.SetProcessCwd(containerCwd)
	if err := setupWorkingDirectory(mountPoint, mountLabel, containerCwd); err != nil {
		if err1 := s.StorageRuntimeServer().StopContainer(containerID); err1 != nil {
			return nil, fmt.Errorf("can't umount container after cwd error %v: %v", err, err1)
		}
		return nil, err
	}

	var secretMounts []rspec.Mount
	if len(s.config.DefaultMounts) > 0 {
		// This option has been deprecated, once it is removed in the later versions, delete the server/secrets.go file as well
		logrus.Warnf("--default-mounts has been deprecated and will be removed in future versions. Add mounts to either %q or %q", secrets.DefaultMountsFile, secrets.OverrideMountsFile)
		var err error
		secretMounts, err = addSecretsBindMounts(mountLabel, containerInfo.RunDir, s.config.DefaultMounts, specgen)
		if err != nil {
			return nil, fmt.Errorf("failed to mount secrets: %v", err)
		}
	}
	// Add secrets from the default and override mounts.conf files
	secretMounts = append(secretMounts, secrets.SecretMounts(mountLabel, containerInfo.RunDir, s.config.DefaultMountsFile)...)

	mounts := []rspec.Mount{}
	mounts = append(mounts, ociMounts...)
	mounts = append(mounts, volumeMounts...)
	mounts = append(mounts, secretMounts...)

	sort.Sort(orderedMounts(mounts))

	for _, m := range mounts {
		mnt = rspec.Mount{
			Type:        "bind",
			Source:      m.Source,
			Destination: m.Destination,
			Options:     append(m.Options, "bind"),
		}
		specgen.AddMount(mnt)
	}

	annotations := map[string]string{}
	for key, value := range containerConfig.GetAnnotations() {
		annotations[key] = value
	}
	for key, value := range sb.Annotations() {
		annotations[key] = value
	}
	if s.ContainerServer.Hooks != nil {
		if _, err := s.ContainerServer.Hooks.Hooks(specgen.Config, annotations, len(containerConfig.GetMounts()) > 0); err != nil {
			return nil, err
		}
	}

	// Setup user and groups
	if linux != nil {
		if err = setupContainerUser(&specgen, mountPoint, mountLabel, containerInfo.RunDir, linux.GetSecurityContext(), containerImageConfig); err != nil {
			return nil, err
		}
	}

	// Set up pids limit if pids cgroup is mounted
	if findCgroupMountpoint("pids") == nil {
		specgen.SetLinuxResourcesPidsLimit(s.config.PidsLimit)
	}

	// by default, the root path is an empty string. set it now.
	specgen.SetRootPath(mountPoint)

	crioAnnotations := specgen.Spec().Annotations

	container, err := oci.NewContainer(containerID, containerName, containerInfo.RunDir, logPath, sb.NetNs().Path(), labels, crioAnnotations, kubeAnnotations, image, imageName, imageRef, metadata, sb.ID(), containerConfig.Tty, containerConfig.Stdin, containerConfig.StdinOnce, sb.Privileged(), sb.Trusted(), sb.RuntimeHandler(), containerInfo.Dir, created, containerImageConfig.Config.StopSignal)
	if err != nil {
		return nil, err
	}

	container.SetIDMappings(containerIDMappings)
	if s.defaultIDMappings != nil && !s.defaultIDMappings.Empty() {
		userNsPath := sb.UserNsPath()
		rootPair := s.defaultIDMappings.RootPair()

		if err := specgen.AddOrReplaceLinuxNamespace(string(rspec.UserNamespace), userNsPath); err != nil {
			return nil, err
		}
		for _, uidmap := range s.defaultIDMappings.UIDs() {
			specgen.AddLinuxUIDMapping(uint32(uidmap.HostID), uint32(uidmap.ContainerID), uint32(uidmap.Size))
		}
		for _, gidmap := range s.defaultIDMappings.GIDs() {
			specgen.AddLinuxGIDMapping(uint32(gidmap.HostID), uint32(gidmap.ContainerID), uint32(gidmap.Size))
		}
		err = s.configureIntermediateNamespace(&specgen, container, sb.InfraContainer())
		if err != nil {
			return nil, err
		}

		if sb.ResolvPath() != "" {
			err = os.Chown(sb.ResolvPath(), rootPair.UID, rootPair.GID)
			if err != nil {
				return nil, err
			}
		}

		defer func() {
			if err != nil {
				os.RemoveAll(container.IntermediateMountPoint())
			}
		}()
	}

	if os.Getenv("_CRIO_ROOTLESS") != "" {
		if err := makeOCIConfigurationRootless(&specgen); err != nil {
			return nil, err
		}
	}

	saveOptions := generate.ExportOptions{}
	if err = specgen.SaveToFile(filepath.Join(containerInfo.Dir, "config.json"), saveOptions); err != nil {
		return nil, err
	}
	if err = specgen.SaveToFile(filepath.Join(containerInfo.RunDir, "config.json"), saveOptions); err != nil {
		return nil, err
	}

	container.SetSpec(specgen.Spec())
	container.SetMountPoint(mountPoint)
	container.SetSeccompProfilePath(spp)

	for _, cv := range containerVolumes {
		container.AddVolume(cv)
	}

	return container, nil
}

func setupWorkingDirectory(rootfs, mountLabel, containerCwd string) error {
	fp, err := symlink.FollowSymlinkInScope(filepath.Join(rootfs, containerCwd), rootfs)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(fp, 0755); err != nil {
		return err
	}
	if mountLabel != "" {
		if err1 := securityLabel(fp, mountLabel, false); err1 != nil {
			return err1
		}
	}
	return nil
}

func setOCIBindMountsPrivileged(g *generate.Generator) {
	spec := g.Spec()
	// clear readonly for /sys and cgroup
	for i, m := range spec.Mounts {
		if spec.Mounts[i].Destination == "/sys" {
			clearReadOnly(&spec.Mounts[i])
		}
		if m.Type == "cgroup" {
			clearReadOnly(&spec.Mounts[i])
		}
	}
	spec.Linux.ReadonlyPaths = nil
	spec.Linux.MaskedPaths = nil
}

func clearReadOnly(m *rspec.Mount) {
	var opt []string
	for _, o := range m.Options {
		if o != "ro" {
			opt = append(opt, o)
		}
	}
	m.Options = append(opt, "rw")
}

func addOCIBindMounts(mountLabel string, containerConfig *pb.ContainerConfig, specgen *generate.Generator, bindMountPrefix string) ([]oci.ContainerVolume, []rspec.Mount, error) {
	volumes := []oci.ContainerVolume{}
	ociMounts := []rspec.Mount{}
	mounts := containerConfig.GetMounts()

	// Sort mounts in number of parts. This ensures that high level mounts don't
	// shadow other mounts.
	sort.Sort(criOrderedMounts(mounts))

	// Copy all mounts from default mounts, except for
	// - mounts overridden by supplied mount;
	// - all mounts under /dev if a supplied /dev is present.
	mountSet := make(map[string]struct{})
	for _, m := range mounts {
		mountSet[filepath.Clean(m.ContainerPath)] = struct{}{}
	}
	defaultMounts := specgen.Mounts()
	specgen.ClearMounts()
	for _, m := range defaultMounts {
		dst := filepath.Clean(m.Destination)
		if _, ok := mountSet[dst]; ok {
			// filter out mount overridden by a supplied mount
			continue
		}
		if _, mountDev := mountSet["/dev"]; mountDev && strings.HasPrefix(dst, "/dev/") {
			// filter out everything under /dev if /dev is a supplied mount
			continue
		}
		specgen.AddMount(m)
	}

	for _, mount := range mounts {
		dest := mount.GetContainerPath()
		if dest == "" {
			return nil, nil, fmt.Errorf("Mount.ContainerPath is empty")
		}

		if mount.HostPath == "" {
			return nil, nil, fmt.Errorf("Mount.HostPath is empty")
		}
		src := filepath.Join(bindMountPrefix, mount.GetHostPath())

		resolvedSrc, err := resolveSymbolicLink(src, bindMountPrefix)
		if err == nil {
			src = resolvedSrc
		} else {
			if !os.IsNotExist(err) {
				return nil, nil, fmt.Errorf("failed to resolve symlink %q: %v", src, err)
			} else if err = os.MkdirAll(src, 0755); err != nil {
				return nil, nil, fmt.Errorf("Failed to mkdir %s: %s", src, err)
			}
		}

		options := []string{"rw"}
		if mount.Readonly {
			options = []string{"ro"}
		}
		options = append(options, "rbind")

		// mount propagation
		mountInfos, err := dockermounts.GetMounts(nil)
		if err != nil {
			return nil, nil, err
		}
		switch mount.GetPropagation() {
		case pb.MountPropagation_PROPAGATION_PRIVATE:
			options = append(options, "rprivate")
			// Since default root propagation in runc is rprivate ignore
			// setting the root propagation
		case pb.MountPropagation_PROPAGATION_BIDIRECTIONAL:
			if err := ensureShared(src, mountInfos); err != nil {
				return nil, nil, err
			}
			options = append(options, "rshared")
			specgen.SetLinuxRootPropagation("rshared")
		case pb.MountPropagation_PROPAGATION_HOST_TO_CONTAINER:
			if err := ensureSharedOrSlave(src, mountInfos); err != nil {
				return nil, nil, err
			}
			options = append(options, "rslave")
			if specgen.Spec().Linux.RootfsPropagation != "rshared" &&
				specgen.Spec().Linux.RootfsPropagation != "rslave" {
				specgen.SetLinuxRootPropagation("rslave")
			}
		default:
			logrus.Warnf("Unknown propagation mode for hostPath %q", mount.HostPath)
			options = append(options, "rprivate")
		}

		if mount.SelinuxRelabel {
			if err := securityLabel(src, mountLabel, false); err != nil {
				return nil, nil, err
			}
		}

		volumes = append(volumes, oci.ContainerVolume{
			ContainerPath: dest,
			HostPath:      src,
			Readonly:      mount.Readonly,
		})

		ociMounts = append(ociMounts, rspec.Mount{
			Source:      src,
			Destination: dest,
			Options:     options,
		})
	}

	return volumes, ociMounts, nil
}

func getDevicesFromConfig(config Config) ([]rspec.LinuxDevice, error) {
	var linuxdevs []rspec.LinuxDevice
	for _, d := range config.RuntimeConfig.AdditionalDevices {
		src, dst, permissions, err := createconfig.ParseDevice(d)
		if err != nil {
			return nil, err
		}
		dev, err := devices.DeviceFromPath(src, permissions)
		if err != nil {
			return nil, errors.Wrapf(err, "%s is not a valid device", src)
		}
		dev.Path = dst
		linuxdev := rspec.LinuxDevice{
			Path:     dev.Path,
			Type:     string(dev.Type),
			Major:    dev.Major,
			Minor:    dev.Minor,
			FileMode: &dev.FileMode,
			UID:      &dev.Uid,
			GID:      &dev.Gid,
		}
		linuxdevs = append(linuxdevs, linuxdev)
	}
	return linuxdevs, nil
}
