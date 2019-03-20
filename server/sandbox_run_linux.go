// +build linux

package server

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/containers/storage"
	"github.com/kubernetes-sigs/cri-o/lib/sandbox"
	"github.com/kubernetes-sigs/cri-o/oci"
	"github.com/kubernetes-sigs/cri-o/pkg/annotations"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	spec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/opencontainers/selinux/go-selinux/label"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
	pb "k8s.io/kubernetes/pkg/kubelet/apis/cri/runtime/v1alpha2"
	"k8s.io/kubernetes/pkg/kubelet/leaky"
	"k8s.io/kubernetes/pkg/kubelet/types"
)

func (s *Server) runPodSandbox(ctx context.Context, req *pb.RunPodSandboxRequest) (resp *pb.RunPodSandboxResponse, err error) {
	const operation = "run_pod_sandbox"
	defer func() {
		recordOperation(operation, time.Now())
		recordError(operation, err)
	}()

	s.updateLock.RLock()
	defer s.updateLock.RUnlock()

	if req.GetConfig().GetMetadata() == nil {
		return nil, fmt.Errorf("CreateContainerRequest.ContainerConfig.Metadata is nil")
	}

	logrus.Debugf("RunPodSandboxRequest %+v", req)
	// we need to fill in the container name, as it is not present in the request. Luckily, it is a constant.
	logrus.Infof("Attempting to run pod sandbox with infra container: %s%s", translateLabelsToDescription(req.GetConfig().GetLabels()), leaky.PodInfraContainerName)
	var processLabel, mountLabel, resolvPath string
	// process req.Name
	kubeName := req.GetConfig().GetMetadata().GetName()
	if kubeName == "" {
		return nil, fmt.Errorf("PodSandboxConfig.Name should not be empty")
	}

	namespace := req.GetConfig().GetMetadata().GetNamespace()
	attempt := req.GetConfig().GetMetadata().GetAttempt()

	id, name, err := s.generatePodIDandName(req.GetConfig())
	if err != nil {
		if strings.Contains(err.Error(), "already reserved for pod") {
			matches := conflictRE.FindStringSubmatch(err.Error())
			if len(matches) != 2 {
				return nil, err
			}
			dupID := matches[1]
			if _, err := s.StopPodSandbox(ctx, &pb.StopPodSandboxRequest{PodSandboxId: dupID}); err != nil {
				return nil, err
			}
			if _, err := s.RemovePodSandbox(ctx, &pb.RemovePodSandboxRequest{PodSandboxId: dupID}); err != nil {
				return nil, err
			}
			id, name, err = s.generatePodIDandName(req.GetConfig())
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	defer func() {
		if err != nil {
			s.ReleasePodName(name)
		}
	}()

	_, containerName, err := s.generateContainerIDandNameForSandbox(req.GetConfig())
	if err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			s.ReleaseContainerName(containerName)
		}
	}()

	podContainer, err := s.StorageRuntimeServer().CreatePodSandbox(s.ImageContext(),
		name, id,
		s.config.PauseImage, "",
		containerName,
		req.GetConfig().GetMetadata().GetName(),
		req.GetConfig().GetMetadata().GetUid(),
		namespace,
		attempt,
		s.defaultIDMappings)
	if errors.Cause(err) == storage.ErrDuplicateName {
		return nil, fmt.Errorf("pod sandbox with name %q already exists", name)
	}
	if err != nil {
		return nil, fmt.Errorf("error creating pod sandbox with name %q: %v", name, err)
	}
	defer func() {
		if err != nil {
			if err2 := s.StorageRuntimeServer().RemovePodSandbox(id); err2 != nil {
				logrus.Warnf("couldn't cleanup pod sandbox %q: %v", id, err2)
			}
		}
	}()

	// TODO: factor generating/updating the spec into something other projects can vendor

	// creates a spec Generator with the default spec.
	g, err := generate.New("linux")
	if err != nil {
		return nil, err
	}
	g.HostSpecific = true
	g.ClearProcessRlimits()

	ulimits, err := getUlimitsFromConfig(s.config)
	if err != nil {
		return nil, err
	}
	for _, u := range ulimits {
		g.AddProcessRlimits(u.name, u.hard, u.soft)
	}

	// setup defaults for the pod sandbox
	g.SetRootReadonly(true)

	// set DNS options
	if req.GetConfig().GetDnsConfig() != nil {
		dnsServers := req.GetConfig().GetDnsConfig().Servers
		dnsSearches := req.GetConfig().GetDnsConfig().Searches
		dnsOptions := req.GetConfig().GetDnsConfig().Options
		resolvPath = fmt.Sprintf("%s/resolv.conf", podContainer.RunDir)
		err = parseDNSOptions(dnsServers, dnsSearches, dnsOptions, resolvPath)
		if err != nil {
			err1 := removeFile(resolvPath)
			if err1 != nil {
				err = err1
				return nil, fmt.Errorf("%v; failed to remove %s: %v", err, resolvPath, err1)
			}
			return nil, err
		}
		if err := label.Relabel(resolvPath, mountLabel, false); err != nil && err != unix.ENOTSUP {
			return nil, err
		}
		mnt := runtimespec.Mount{
			Type:        "bind",
			Source:      resolvPath,
			Destination: "/etc/resolv.conf",
			Options:     []string{"ro", "bind", "nodev", "nosuid", "noexec"},
		}
		g.AddMount(mnt)
	}

	// add metadata
	metadata := req.GetConfig().GetMetadata()
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}

	// add labels
	labels := req.GetConfig().GetLabels()

	if err := validateLabels(labels); err != nil {
		return nil, err
	}

	// Add special container name label for the infra container
	labelsJSON := []byte{}
	if labels != nil {
		labels[types.KubernetesContainerNameLabel] = leaky.PodInfraContainerName
		labelsJSON, err = json.Marshal(labels)
		if err != nil {
			return nil, err
		}
	}

	// add annotations
	kubeAnnotations := req.GetConfig().GetAnnotations()
	kubeAnnotationsJSON, err := json.Marshal(kubeAnnotations)
	if err != nil {
		return nil, err
	}

	// set log directory
	logDir := req.GetConfig().GetLogDirectory()
	if logDir == "" {
		logDir = filepath.Join(s.config.LogDir, id)
	}
	if err = os.MkdirAll(logDir, 0700); err != nil {
		return nil, err
	}
	// This should always be absolute from k8s.
	if !filepath.IsAbs(logDir) {
		return nil, fmt.Errorf("requested logDir for sbox id %s is a relative path: %s", id, logDir)
	}

	privileged := s.privilegedSandbox(req)

	// Add capabilities from crio.conf if default_capabilities is defined
	capabilities := &pb.Capability{}
	if s.config.DefaultCapabilities != nil {
		g.ClearProcessCapabilities()
		capabilities.AddCapabilities = append(capabilities.AddCapabilities, s.config.DefaultCapabilities...)
	}
	if err := setupCapabilities(&g, capabilities); err != nil {
		return nil, err
	}

	securityContext := req.GetConfig().GetLinux().GetSecurityContext()
	if securityContext == nil {
		logrus.Warn("no security context found in config.")
	}

	nsOptsJSON, err := json.Marshal(securityContext.GetNamespaceOptions())
	if err != nil {
		return nil, err
	}

	processLabel, mountLabel, err = getSELinuxLabels(securityContext.GetSelinuxOptions(), privileged)
	if err != nil {
		return nil, err
	}

	hostIPC := securityContext.GetNamespaceOptions().GetIpc() == pb.NamespaceMode_NODE

	// Don't use SELinux separation with Host Pid or IPC Namespace or privileged.
	if securityContext.GetNamespaceOptions().GetPid() == pb.NamespaceMode_NODE || hostIPC {
		processLabel, mountLabel = "", ""
	}
	g.SetProcessSelinuxLabel(processLabel)
	g.SetLinuxMountLabel(mountLabel)

	// Remove the default /dev/shm mount to ensure we overwrite it
	g.RemoveMount("/dev/shm")

	containerKubeConfig := &pb.ContainerConfig{}
	if s.config.PauseCommand != "" {
		containerKubeConfig.Command = []string{s.config.PauseCommand}
	}
	processArgs, err := buildOCIProcessArgs(containerKubeConfig, &podContainer.Config.Config)
	if err != nil {
		return nil, err
	}
	if len(processArgs) == 0 {
		g.Spec().Process = nil
	} else {
		g.SetProcessArgs(processArgs)
	}

	// create shm mount for the pod containers.
	var shmPath string
	if hostIPC {
		shmPath = "/dev/shm"
	} else {
		shmPath, err = setupShm(podContainer.RunDir, mountLabel)
		if err != nil {
			return nil, err
		}
		defer func() {
			if err != nil {
				if err2 := unix.Unmount(shmPath, unix.MNT_DETACH); err2 != nil {
					logrus.Warnf("failed to unmount shm for pod: %v", err2)
				}
			}
		}()
	}

	mnt := runtimespec.Mount{
		Type:        "bind",
		Source:      shmPath,
		Destination: "/dev/shm",
		Options:     []string{"rw", "bind"},
	}
	// bind mount the pod shm
	g.AddMount(mnt)

	err = s.setPodSandboxMountLabel(id, mountLabel)
	if err != nil {
		return nil, err
	}

	if err = s.CtrIDIndex().Add(id); err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			if err2 := s.CtrIDIndex().Delete(id); err2 != nil {
				logrus.Warnf("couldn't delete ctr id %s from idIndex", id)
			}
		}
	}()

	// set log path inside log directory
	logPath := filepath.Join(logDir, id+".log")

	// Handle https://issues.k8s.io/44043
	if err := ensureSaneLogPath(logPath); err != nil {
		return nil, err
	}

	hostNetwork := securityContext.GetNamespaceOptions().GetNetwork() == pb.NamespaceMode_NODE

	hostname, err := getHostname(id, req.GetConfig().Hostname, hostNetwork)
	if err != nil {
		return nil, err
	}
	g.SetHostname(hostname)

	// validate the runtime handler
	runtimeHandler, err := s.runtimeHandler(req)
	if err != nil {
		return nil, err
	}

	trusted := s.trustedSandbox(req)
	g.AddAnnotation(annotations.Metadata, string(metadataJSON))
	g.AddAnnotation(annotations.Labels, string(labelsJSON))
	g.AddAnnotation(annotations.Annotations, string(kubeAnnotationsJSON))
	g.AddAnnotation(annotations.LogPath, logPath)
	g.AddAnnotation(annotations.Name, name)
	g.AddAnnotation(annotations.Namespace, namespace)
	g.AddAnnotation(annotations.ContainerType, annotations.ContainerTypeSandbox)
	g.AddAnnotation(annotations.SandboxID, id)
	g.AddAnnotation(annotations.ContainerName, containerName)
	g.AddAnnotation(annotations.ContainerID, id)
	g.AddAnnotation(annotations.ShmPath, shmPath)
	g.AddAnnotation(annotations.PrivilegedRuntime, fmt.Sprintf("%v", privileged))
	g.AddAnnotation(annotations.TrustedSandbox, fmt.Sprintf("%v", trusted))
	g.AddAnnotation(annotations.RuntimeHandler, runtimeHandler)
	g.AddAnnotation(annotations.ResolvPath, resolvPath)
	g.AddAnnotation(annotations.HostName, hostname)
	g.AddAnnotation(annotations.NamespaceOptions, string(nsOptsJSON))
	g.AddAnnotation(annotations.KubeName, kubeName)
	g.AddAnnotation(annotations.HostNetwork, fmt.Sprintf("%v", hostNetwork))
	if podContainer.Config.Config.StopSignal != "" {
		// this key is defined in image-spec conversion document at https://github.com/opencontainers/image-spec/pull/492/files#diff-8aafbe2c3690162540381b8cdb157112R57
		g.AddAnnotation("org.opencontainers.image.stopSignal", podContainer.Config.Config.StopSignal)
	}

	created := time.Now()
	g.AddAnnotation(annotations.Created, created.Format(time.RFC3339Nano))

	portMappings := convertPortMappings(req.GetConfig().GetPortMappings())
	portMappingsJSON, err := json.Marshal(portMappings)
	if err != nil {
		return nil, err
	}
	g.AddAnnotation(annotations.PortMappings, string(portMappingsJSON))

	// setup cgroup settings
	cgroupParent := req.GetConfig().GetLinux().GetCgroupParent()
	if cgroupParent != "" {
		if s.config.CgroupManager == oci.SystemdCgroupsManager {
			if len(cgroupParent) <= 6 || !strings.HasSuffix(path.Base(cgroupParent), ".slice") {
				return nil, fmt.Errorf("cri-o configured with systemd cgroup manager, but did not receive slice as parent: %s", cgroupParent)
			}
			cgPath, err := convertCgroupFsNameToSystemd(cgroupParent)
			if err != nil {
				return nil, err
			}
			g.SetLinuxCgroupsPath(cgPath + ":" + "crio" + ":" + id)
			cgroupParent = cgPath
		} else {
			if strings.HasSuffix(path.Base(cgroupParent), ".slice") {
				return nil, fmt.Errorf("cri-o configured with cgroupfs cgroup manager, but received systemd slice as parent: %s", cgroupParent)
			}
			cgPath := filepath.Join(cgroupParent, scopePrefix+"-"+id)
			g.SetLinuxCgroupsPath(cgPath)
		}
	}
	g.AddAnnotation(annotations.CgroupParent, cgroupParent)

	if s.defaultIDMappings != nil && !s.defaultIDMappings.Empty() {
		g.AddOrReplaceLinuxNamespace(spec.UserNamespace, "")
		for _, uidmap := range s.defaultIDMappings.UIDs() {
			g.AddLinuxUIDMapping(uint32(uidmap.HostID), uint32(uidmap.ContainerID), uint32(uidmap.Size))
		}
		for _, gidmap := range s.defaultIDMappings.GIDs() {
			g.AddLinuxGIDMapping(uint32(gidmap.HostID), uint32(gidmap.ContainerID), uint32(gidmap.Size))
		}
	}

	sb, err := sandbox.New(id, namespace, name, kubeName, logDir, labels, kubeAnnotations, processLabel, mountLabel, metadata, shmPath, cgroupParent, privileged, trusted, runtimeHandler, resolvPath, hostname, portMappings, hostNetwork)
	if err != nil {
		return nil, err
	}

	s.addSandbox(sb)
	defer func() {
		if err != nil {
			s.removeSandbox(id)
		}
	}()

	if err = s.PodIDIndex().Add(id); err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			if err := s.PodIDIndex().Delete(id); err != nil {
				logrus.Warnf("couldn't delete pod id %s from idIndex", id)
			}
		}
	}()

	for k, v := range kubeAnnotations {
		g.AddAnnotation(k, v)
	}
	for k, v := range labels {
		g.AddAnnotation(k, v)
	}

	// Add default sysctls given in crio.conf
	for _, defaultSysctl := range s.config.DefaultSysctls {
		split := strings.SplitN(defaultSysctl, "=", 2)
		if len(split) == 2 {
			if err := validateSysctl(split[0], hostNetwork, hostIPC); err != nil {
				logrus.Warnf("sysctl not valid %q: %v - skipping...", defaultSysctl, err)
				continue
			}
			g.AddLinuxSysctl(split[0], split[1])
			continue
		}
		logrus.Warnf("sysctl %q not of the format sysctl_name=value", defaultSysctl)
	}
	// extract linux sysctls from annotations and pass down to oci runtime
	// Will override any duplicate default systcl from crio.conf
	for key, value := range req.GetConfig().GetLinux().GetSysctls() {
		g.AddLinuxSysctl(key, value)
	}

	// Set OOM score adjust of the infra container to be very low
	// so it doesn't get killed.
	g.SetProcessOOMScoreAdj(PodInfraOOMAdj)

	g.SetLinuxResourcesCPUShares(PodInfraCPUshares)

	// set up namespaces
	if hostNetwork {
		err = g.RemoveLinuxNamespace(string(runtimespec.NetworkNamespace))
		if err != nil {
			return nil, err
		}
	} else {
		if s.config.Config.ManageNetworkNSLifecycle {
			// Create the sandbox network namespace
			if err = sb.NetNsCreate(nil); err != nil {
				return nil, err
			}

			defer func() {
				if err == nil {
					return
				}

				if netnsErr := sb.NetNsRemove(); netnsErr != nil {
					logrus.Warnf("Failed to remove networking namespace: %v", netnsErr)
				}
			}()

			// Pass the created namespace path to the runtime
			err = g.AddOrReplaceLinuxNamespace(string(runtimespec.NetworkNamespace), sb.NetNsPath())
			if err != nil {
				return nil, err
			}
		}
	}

	if securityContext.GetNamespaceOptions().GetPid() == pb.NamespaceMode_NODE {
		err = g.RemoveLinuxNamespace(string(runtimespec.PIDNamespace))
		if err != nil {
			return nil, err
		}
	}

	if securityContext.GetNamespaceOptions().GetIpc() == pb.NamespaceMode_NODE {
		err = g.RemoveLinuxNamespace(string(runtimespec.IPCNamespace))
		if err != nil {
			return nil, err
		}
	}

	if !s.seccompEnabled {
		g.Spec().Linux.Seccomp = nil
	}

	saveOptions := generate.ExportOptions{}
	mountPoint, err := s.StorageRuntimeServer().StartContainer(id)
	if err != nil {
		return nil, fmt.Errorf("failed to mount container %s in pod sandbox %s(%s): %v", containerName, sb.Name(), id, err)
	}
	g.AddAnnotation(annotations.MountPoint, mountPoint)

	hostnamePath := fmt.Sprintf("%s/hostname", podContainer.RunDir)
	if err := ioutil.WriteFile(hostnamePath, []byte(hostname+"\n"), 0644); err != nil {
		return nil, err
	}
	if err := label.Relabel(hostnamePath, mountLabel, false); err != nil && err != unix.ENOTSUP {
		return nil, err
	}
	mnt = runtimespec.Mount{
		Type:        "bind",
		Source:      hostnamePath,
		Destination: "/etc/hostname",
		Options:     []string{"ro", "bind", "nodev", "nosuid", "noexec"},
	}
	g.AddMount(mnt)
	g.AddAnnotation(annotations.HostnamePath, hostnamePath)
	sb.AddHostnamePath(hostnamePath)

	container, err := oci.NewContainer(id, containerName, podContainer.RunDir, logPath, sb.NetNs().Path(), labels, g.Spec().Annotations, kubeAnnotations, "", "", "", nil, id, false, false, false, sb.Privileged(), sb.Trusted(), sb.RuntimeHandler(), podContainer.Dir, created, podContainer.Config.Config.StopSignal)
	if err != nil {
		return nil, err
	}
	container.SetMountPoint(mountPoint)

	container.SetIDMappings(s.defaultIDMappings)

	if s.defaultIDMappings != nil && !s.defaultIDMappings.Empty() {
		if securityContext.GetNamespaceOptions().GetIpc() == pb.NamespaceMode_NODE {
			g.RemoveMount("/dev/mqueue")
			mqueue := runtimespec.Mount{
				Type:        "bind",
				Source:      "/dev/mqueue",
				Destination: "/dev/mqueue",
				Options:     []string{"rw", "rbind", "nodev", "nosuid", "noexec"},
			}
			g.AddMount(mqueue)
		}
		if hostNetwork {
			g.RemoveMount("/sys")
			g.RemoveMount("/sys/cgroup")
			sysMnt := spec.Mount{
				Destination: "/sys",
				Type:        "bind",
				Source:      "/sys",
				Options:     []string{"nosuid", "noexec", "nodev", "ro", "rbind"},
			}
			g.AddMount(sysMnt)
		}
		if securityContext.GetNamespaceOptions().GetPid() == pb.NamespaceMode_NODE {
			g.RemoveMount("/proc")
			proc := runtimespec.Mount{
				Type:        "bind",
				Source:      "/proc",
				Destination: "/proc",
				Options:     []string{"rw", "rbind", "nodev", "nosuid", "noexec"},
			}
			g.AddMount(proc)
		}

		err = s.configureIntermediateNamespace(&g, container, nil)
		if err != nil {
			return nil, err
		}
		defer func() {
			if err != nil {
				os.RemoveAll(container.IntermediateMountPoint())
			}
		}()
	} else {
		g.SetRootPath(mountPoint)
	}

	if os.Getenv("_CRIO_ROOTLESS") != "" {
		if err := makeOCIConfigurationRootless(&g); err != nil {
			return nil, err
		}
	}

	container.SetSpec(g.Spec())

	sb.SetInfraContainer(container)

	var ip string
	var result cnitypes.Result

	if s.config.Config.ManageNetworkNSLifecycle {
		ip, result, err = s.networkStart(sb)
		if err != nil {
			return nil, err
		}
		if result != nil {
			g.AddAnnotation(annotations.CNIResult, result.String())
		}
		defer func() {
			if err != nil {
				s.networkStop(sb)
			}
		}()
	}

	g.AddAnnotation(annotations.IP, ip)
	sb.AddIP(ip)
	sb.SetNamespaceOptions(securityContext.GetNamespaceOptions())

	spp := req.GetConfig().GetLinux().GetSecurityContext().GetSeccompProfilePath()
	g.AddAnnotation(annotations.SeccompProfilePath, spp)
	sb.SetSeccompProfilePath(spp)
	if !privileged {
		if err = s.setupSeccomp(&g, spp); err != nil {
			return nil, err
		}
	}

	err = g.SaveToFile(filepath.Join(podContainer.Dir, "config.json"), saveOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to save template configuration for pod sandbox %s(%s): %v", sb.Name(), id, err)
	}
	if err = g.SaveToFile(filepath.Join(podContainer.RunDir, "config.json"), saveOptions); err != nil {
		return nil, fmt.Errorf("failed to write runtime configuration for pod sandbox %s(%s): %v", sb.Name(), id, err)
	}

	s.addInfraContainer(container)
	defer func() {
		if err != nil {
			s.removeInfraContainer(container)
		}
	}()

	if err = s.createContainerPlatform(container, nil, sb.CgroupParent()); err != nil {
		return nil, err
	}

	if err = s.Runtime().StartContainer(container); err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			// Clean-up steps from RemovePodSanbox
			timeout := int64(10)
			if err2 := s.Runtime().StopContainer(ctx, container, timeout); err2 != nil {
				logrus.Warnf("failed to stop container %s: %v", container.Name(), err2)
			}
			if err2 := s.Runtime().WaitContainerStateStopped(ctx, container); err2 != nil {
				logrus.Warnf("failed to get container 'stopped' status %s in pod sandbox %s: %v", container.Name(), sb.ID(), err2)
			}
			if err2 := s.Runtime().DeleteContainer(container); err2 != nil {
				logrus.Warnf("failed to delete container %s in pod sandbox %s: %v", container.Name(), sb.ID(), err2)
			}
			s.ContainerStateToDisk(container)
		}
	}()

	s.ContainerStateToDisk(container)

	if !s.config.Config.ManageNetworkNSLifecycle {
		ip, _, err = s.networkStart(sb)
		if err != nil {
			return nil, err
		}
		defer func() {
			if err != nil {
				s.networkStop(sb)
			}
		}()
	}
	sb.AddIP(ip)

	sb.SetCreated()

	logrus.Infof("Ran pod sandbox with infra container: %s", container.Description())
	resp = &pb.RunPodSandboxResponse{PodSandboxId: id}
	logrus.Debugf("RunPodSandboxResponse: %+v", resp)
	return resp, nil
}

func setupShm(podSandboxRunDir, mountLabel string) (shmPath string, err error) {
	shmPath = filepath.Join(podSandboxRunDir, "shm")
	if err = os.Mkdir(shmPath, 0700); err != nil {
		return "", err
	}
	shmOptions := "mode=1777,size=" + strconv.Itoa(sandbox.DefaultShmSize)
	if err = unix.Mount("shm", shmPath, "tmpfs", unix.MS_NOEXEC|unix.MS_NOSUID|unix.MS_NODEV,
		label.FormatMountLabel(shmOptions, mountLabel)); err != nil {
		return "", fmt.Errorf("failed to mount shm tmpfs for pod: %v", err)
	}
	return shmPath, nil
}
