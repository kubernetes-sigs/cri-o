// +build linux

package server

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/current"
	"github.com/containers/libpod/v2/pkg/annotations"
	"github.com/containers/libpod/v2/pkg/rootless"
	selinux "github.com/containers/libpod/v2/pkg/selinux"
	"github.com/containers/storage"
	"github.com/containers/storage/pkg/idtools"
	"github.com/cri-o/cri-o/internal/config/node"
	"github.com/cri-o/cri-o/internal/lib"
	libsandbox "github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/log"
	oci "github.com/cri-o/cri-o/internal/oci"
	libconfig "github.com/cri-o/cri-o/pkg/config"
	"github.com/cri-o/cri-o/pkg/sandbox"
	"github.com/cri-o/cri-o/utils"
	json "github.com/json-iterator/go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	spec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/opencontainers/selinux/go-selinux/label"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
	pb "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/kubernetes/pkg/kubelet/leaky"
	"k8s.io/kubernetes/pkg/kubelet/types"
)

// DefaultUserNSSize is the default size for the user namespace created
const DefaultUserNSSize = 65536

// addToMappingsIfMissing ensures the specified id is mapped from the host.
func addToMappingsIfMissing(ids []idtools.IDMap, id int64) []idtools.IDMap {
	firstAvailable := int(0)
	for _, r := range ids {
		if int(id) >= r.HostID && int(id) < r.HostID+r.Size {
			// Already present, nothing to do
			return ids
		}
		if r.ContainerID+r.Size > firstAvailable {
			firstAvailable = r.ContainerID + r.Size
		}
	}
	newMapping := idtools.IDMap{
		ContainerID: firstAvailable,
		HostID:      int(id),
		Size:        1,
	}
	return append(ids, newMapping)
}

func (s *Server) configureSandboxIDMappings(mode string, sc *pb.LinuxSandboxSecurityContext) (*storage.IDMappingOptions, error) {
	// Ignore the annotation if not explicitly set in the config file.
	if !s.config.AllowUsernsAnnotation || mode == "" {
		// No mode specified but mappings set in the config file, let's use them.
		if s.defaultIDMappings != nil {
			uids := s.defaultIDMappings.UIDs()
			gids := s.defaultIDMappings.GIDs()
			return &storage.IDMappingOptions{UIDMap: uids, GIDMap: gids}, nil
		}
		return nil, nil
	}

	// expect a configuration like: private:uidmapping=0:1000:2000,2000:1000:2000;gidmapping=0:1000:4000,4000:1000:2000
	parts := strings.SplitN(mode, ":", 2)

	values := map[string]string{}
	if len(parts) > 1 {
		for _, r := range strings.Split(parts[1], ";") {
			kv := strings.SplitN(r, "=", 2)
			if len(kv) != 2 {
				return nil, errors.Errorf("invalid argument: %q", r)
			}
			values[kv[0]] = kv[1]
		}
	}

	_, uidMappingsPresent := values["uidmapping"]
	_, gidMappingsPresent := values["gidmapping"]
	// allow these options only if running as root
	if uidMappingsPresent || gidMappingsPresent {
		user := sc.GetRunAsUser()
		if user == nil || user.Value != 0 {
			return nil, errors.New("cannot use uidmapping or gidmapping if not running as root")
		}
	}

	switch parts[0] {
	case "auto":
		const t = "true"
		ret := &storage.IDMappingOptions{
			AutoUserNs: true,
		}
		// If keep-id=true then the UID:GID won't be changed inside of the user namespace and it
		// will map to the same value on the host.
		keepID := values["keep-id"] == t
		// If map-to-root=true then the UID:GID will be mapped to root inside of the user namespace.
		mapToRoot := values["map-to-root"] == t
		if keepID && mapToRoot {
			return nil, errors.Errorf("cannot use both keep-id and map-to-root: %q", mode)
		}
		if v, ok := values["size"]; ok {
			s, err := strconv.Atoi(v)
			if err != nil {
				return nil, err
			}
			ret.AutoUserNsOpts.Size = uint32(s)
		} else {
			ret.AutoUserNsOpts.Size = DefaultUserNSSize
		}
		if v, ok := values["uidmapping"]; ok {
			uids, err := idtools.ParseIDMap(strings.Split(v, ","), "UID")
			if err != nil {
				return nil, err
			}
			ret.AutoUserNsOpts.AdditionalUIDMappings = append(ret.AutoUserNsOpts.AdditionalUIDMappings, uids...)
		}
		if v, ok := values["gidmapping"]; ok {
			gids, err := idtools.ParseIDMap(strings.Split(v, ","), "GID")
			if err != nil {
				return nil, err
			}
			ret.AutoUserNsOpts.AdditionalGIDMappings = append(ret.AutoUserNsOpts.AdditionalGIDMappings, gids...)
		}
		if sc.GetRunAsUser() != nil {
			if keepID || mapToRoot {
				id := 0
				if keepID {
					id = int(sc.GetRunAsUser().Value)
				}
				ret.AutoUserNsOpts.AdditionalUIDMappings = append(
					ret.AutoUserNsOpts.AdditionalUIDMappings,
					idtools.IDMap{
						ContainerID: id,
						HostID:      int(sc.GetRunAsUser().Value),
						Size:        1,
					})
			} else {
				m := addToMappingsIfMissing(ret.AutoUserNsOpts.AdditionalUIDMappings, sc.GetRunAsUser().Value)
				ret.AutoUserNsOpts.AdditionalUIDMappings = m
			}
		}
		if sc.GetRunAsGroup() != nil {
			if keepID || mapToRoot {
				id := 0
				if keepID {
					id = int(sc.GetRunAsGroup().Value)
				}
				ret.AutoUserNsOpts.AdditionalGIDMappings = append(
					ret.AutoUserNsOpts.AdditionalGIDMappings,
					idtools.IDMap{
						ContainerID: id,
						HostID:      int(sc.GetRunAsGroup().Value),
						Size:        1,
					})
			} else {
				m := addToMappingsIfMissing(ret.AutoUserNsOpts.AdditionalGIDMappings, sc.GetRunAsGroup().Value)
				ret.AutoUserNsOpts.AdditionalGIDMappings = m
			}
		}
		for _, g := range sc.GetSupplementalGroups() {
			if keepID {
				ret.AutoUserNsOpts.AdditionalGIDMappings = append(
					ret.AutoUserNsOpts.AdditionalGIDMappings,
					idtools.IDMap{
						ContainerID: int(g),
						HostID:      int(g),
						Size:        1,
					})
			} else {
				m := addToMappingsIfMissing(ret.AutoUserNsOpts.AdditionalGIDMappings, g)
				ret.AutoUserNsOpts.AdditionalGIDMappings = m
			}
		}
		return ret, nil
	case "private":
		var err error
		var uids, gids []idtools.IDMap

		if v, ok := values["uidmapping"]; ok {
			uids, err = idtools.ParseIDMap(strings.Split(v, ","), "UID")
			if err != nil {
				return nil, err
			}
		}
		if v, ok := values["gidmapping"]; ok {
			// both gidmapping and uidmapping are specified
			gids, err = idtools.ParseIDMap(strings.Split(v, ","), "GID")
			if err != nil {
				return nil, err
			}
		}

		if uids == nil && gids == nil {
			if s.defaultIDMappings == nil {
				// no configuration and no global mappings
				return nil, errors.Errorf("userns requested but no userns mappings configured")
			}

			// no configuration specified, so use the global mappings
			uids = s.defaultIDMappings.UIDs()
			gids = s.defaultIDMappings.GIDs()
		} else {
			// one between uids and gids is set, use the same range
			if uids == nil && gids != nil {
				uids = gids
			} else if gids == nil && uids != nil {
				gids = uids
			}
		}
		// make sure the specified users are part of the namespace
		if sc.GetRunAsUser() != nil {
			uids = addToMappingsIfMissing(uids, sc.GetRunAsUser().Value)
		}
		if sc.GetRunAsGroup() != nil {
			gids = addToMappingsIfMissing(gids, sc.GetRunAsGroup().Value)
		}
		for _, g := range sc.GetSupplementalGroups() {
			gids = addToMappingsIfMissing(gids, g)
		}

		return &storage.IDMappingOptions{UIDMap: uids, GIDMap: gids}, nil
	}
	return nil, errors.Errorf("invalid userns mode: %q", mode)
}

func (s *Server) getSandboxIDMappings(sb *libsandbox.Sandbox) (*idtools.IDMappings, error) {
	ic := sb.InfraContainer()
	if ic != nil {
		mappings := ic.IDMappings()
		if mappings != nil {
			return mappings, nil
		}
	}
	if sb.UsernsMode() == "" && s.defaultIDMappings == nil {
		return nil, nil
	}
	// Ignore the annotation if not explicitly set in the config file.
	if s.defaultIDMappings == nil && !s.config.AllowUsernsAnnotation {
		return nil, nil
	}
	if ic == nil {
		return nil, errors.Errorf("infra container not found")
	}

	uids, err := rootless.ReadMappingsProc(fmt.Sprintf("/proc/%d/uid_map", ic.State().Pid))
	if err != nil {
		return nil, err
	}
	gids, err := rootless.ReadMappingsProc(fmt.Sprintf("/proc/%d/gid_map", ic.State().Pid))
	if err != nil {
		return nil, err
	}

	mappings := idtools.NewIDMappingsFromMaps(uids, gids)
	ic.SetIDMappings(mappings)
	return mappings, nil
}

func (s *Server) runPodSandbox(ctx context.Context, req *pb.RunPodSandboxRequest) (resp *pb.RunPodSandboxResponse, retErr error) {
	s.updateLock.RLock()
	defer s.updateLock.RUnlock()

	sbox := sandbox.New(ctx)
	if err := sbox.SetConfig(req.GetConfig()); err != nil {
		return nil, errors.Wrap(err, "setting sandbox config")
	}

	pathsToChown := []string{}

	// we need to fill in the container name, as it is not present in the request. Luckily, it is a constant.
	log.Infof(ctx, "Running pod sandbox: %s%s", translateLabelsToDescription(sbox.Config().GetLabels()), leaky.PodInfraContainerName)

	kubeName := sbox.Config().GetMetadata().GetName()
	namespace := sbox.Config().GetMetadata().GetNamespace()
	attempt := sbox.Config().GetMetadata().GetAttempt()

	if err := sbox.SetNameAndID(); err != nil {
		return nil, errors.Wrap(err, "setting pod sandbox name and id")
	}

	if _, err := s.ReservePodName(sbox.ID(), sbox.Name()); err != nil {
		return nil, errors.Wrap(err, "Kubelet may be retrying requests that are timing out in CRI-O due to system load")
	}

	defer func() {
		if retErr != nil {
			log.Infof(ctx, "runSandbox: releasing pod sandbox name: %s", sbox.Name())
			s.ReleasePodName(sbox.Name())
		}
	}()

	kubeAnnotations := sbox.Config().GetAnnotations()

	usernsMode := kubeAnnotations[lib.UsernsModeAnnotation]

	idMappingsOptions, err := s.configureSandboxIDMappings(usernsMode, sbox.Config().GetLinux().GetSecurityContext())
	if err != nil {
		return nil, err
	}

	containerName, err := s.ReserveSandboxContainerIDAndName(sbox.Config())
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			log.Infof(ctx, "runSandbox: releasing container name: %s", containerName)
			s.ReleaseContainerName(containerName)
		}
	}()

	var labelOptions []string
	securityContext := sbox.Config().GetLinux().GetSecurityContext()
	selinuxConfig := securityContext.GetSelinuxOptions()
	if selinuxConfig != nil {
		labelOptions = utils.GetLabelOptions(selinuxConfig)
	}

	privileged := s.privilegedSandbox(req)

	podContainer, err := s.StorageRuntimeServer().CreatePodSandbox(s.config.SystemContext,
		sbox.Name(), sbox.ID(),
		s.config.PauseImage,
		s.config.PauseImageAuthFile,
		"",
		containerName,
		kubeName,
		sbox.Config().GetMetadata().GetUid(),
		namespace,
		attempt,
		idMappingsOptions,
		labelOptions,
		privileged,
	)

	mountLabel := podContainer.MountLabel
	processLabel := podContainer.ProcessLabel

	if errors.Is(err, storage.ErrDuplicateName) {
		return nil, fmt.Errorf("pod sandbox with name %q already exists", sbox.Name())
	}
	if err != nil {
		return nil, fmt.Errorf("error creating pod sandbox with name %q: %v", sbox.Name(), err)
	}
	defer func() {
		if retErr != nil {
			log.Infof(ctx, "runSandbox: removing pod sandbox from storage: %s", sbox.ID())
			if err2 := s.StorageRuntimeServer().RemovePodSandbox(sbox.ID()); err2 != nil {
				log.Warnf(ctx, "couldn't cleanup pod sandbox %q: %v", sbox.ID(), err2)
			}
		}
	}()

	// set log directory
	logDir := sbox.Config().GetLogDirectory()
	if logDir == "" {
		logDir = filepath.Join(s.config.LogDir, sbox.ID())
	}
	// This should always be absolute from k8s.
	if !filepath.IsAbs(logDir) {
		return nil, fmt.Errorf("requested logDir for sbox id %s is a relative path: %s", sbox.ID(), logDir)
	}
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return nil, err
	}

	var sandboxIDMappings *idtools.IDMappings
	if idMappingsOptions != nil {
		sandboxIDMappings = idtools.NewIDMappingsFromMaps(idMappingsOptions.UIDMap, idMappingsOptions.GIDMap)
	}

	// TODO: factor generating/updating the spec into something other projects can vendor

	// creates a spec Generator with the default spec.
	g, err := generate.New("linux")
	if err != nil {
		return nil, err
	}
	g.HostSpecific = true
	g.ClearProcessRlimits()

	for _, u := range s.config.Ulimits() {
		g.AddProcessRlimits(u.Name, u.Hard, u.Soft)
	}

	// setup defaults for the pod sandbox
	g.SetRootReadonly(true)

	pauseCommand, err := PauseCommand(s.Config(), podContainer.Config)
	if err != nil {
		return nil, err
	}
	g.SetProcessArgs(pauseCommand)

	// set DNS options
	var resolvPath string
	if sbox.Config().GetDnsConfig() != nil {
		dnsServers := sbox.Config().GetDnsConfig().Servers
		dnsSearches := sbox.Config().GetDnsConfig().Searches
		dnsOptions := sbox.Config().GetDnsConfig().Options
		resolvPath = fmt.Sprintf("%s/resolv.conf", podContainer.RunDir)
		err = parseDNSOptions(dnsServers, dnsSearches, dnsOptions, resolvPath)
		if err != nil {
			err1 := removeFile(resolvPath)
			if err1 != nil {
				return nil, fmt.Errorf("%v; failed to remove %s: %v", err, resolvPath, err1)
			}
			return nil, err
		}
		if err := label.Relabel(resolvPath, mountLabel, false); err != nil && !errors.Is(err, unix.ENOTSUP) {
			if err1 := removeFile(resolvPath); err1 != nil {
				return nil, fmt.Errorf("%v; failed to remove %s: %v", err, resolvPath, err1)
			}
		}
		mnt := spec.Mount{
			Type:        "bind",
			Source:      resolvPath,
			Destination: "/etc/resolv.conf",
			Options:     []string{"ro", "bind", "nodev", "nosuid", "noexec"},
		}
		pathsToChown = append(pathsToChown, resolvPath)
		g.AddMount(mnt)
	}

	// add metadata
	metadata := sbox.Config().GetMetadata()
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}

	// add labels
	labels := sbox.Config().GetLabels()

	if err := validateLabels(labels); err != nil {
		return nil, err
	}

	// Add special container name label for the infra container
	if labels != nil {
		labels[types.KubernetesContainerNameLabel] = leaky.PodInfraContainerName
	}
	labelsJSON, err := json.Marshal(labels)
	if err != nil {
		return nil, err
	}

	// add annotations
	kubeAnnotationsJSON, err := json.Marshal(kubeAnnotations)
	if err != nil {
		return nil, err
	}

	// Add capabilities from crio.conf if default_capabilities is defined
	capabilities := &pb.Capability{}
	if s.config.DefaultCapabilities != nil {
		g.ClearProcessCapabilities()
		capabilities.AddCapabilities = append(capabilities.AddCapabilities, s.config.DefaultCapabilities...)
	}
	if err := setupCapabilities(&g, capabilities); err != nil {
		return nil, err
	}

	nsOptsJSON, err := json.Marshal(securityContext.GetNamespaceOptions())
	if err != nil {
		return nil, err
	}

	hostIPC := securityContext.GetNamespaceOptions().GetIpc() == pb.NamespaceMode_NODE
	hostPID := securityContext.GetNamespaceOptions().GetPid() == pb.NamespaceMode_NODE

	// Don't use SELinux separation with Host Pid or IPC Namespace or privileged.
	if hostPID || hostIPC {
		processLabel, mountLabel = "", ""
	}
	g.SetProcessSelinuxLabel(processLabel)
	g.SetLinuxMountLabel(mountLabel)

	// Remove the default /dev/shm mount to ensure we overwrite it
	g.RemoveMount(libsandbox.DevShmPath)

	// create shm mount for the pod containers.
	var shmPath string
	if hostIPC {
		shmPath = libsandbox.DevShmPath
	} else {
		shmPath, err = setupShm(podContainer.RunDir, mountLabel)
		if err != nil {
			return nil, err
		}
		pathsToChown = append(pathsToChown, shmPath)
		defer func() {
			if retErr != nil {
				log.Infof(ctx, "runSandbox: unmounting shmPath for sandbox %s", sbox.ID())
				if err2 := unix.Unmount(shmPath, unix.MNT_DETACH); err2 != nil && err2 != unix.EINVAL {
					log.Warnf(ctx, "failed to unmount shm for pod: %v", err2)
				}
			}
		}()
	}

	mnt := spec.Mount{
		Type:        "bind",
		Source:      shmPath,
		Destination: libsandbox.DevShmPath,
		Options:     []string{"rw", "bind"},
	}
	// bind mount the pod shm
	g.AddMount(mnt)

	err = s.setPodSandboxMountLabel(sbox.ID(), mountLabel)
	if err != nil {
		return nil, err
	}

	if err := s.CtrIDIndex().Add(sbox.ID()); err != nil {
		return nil, err
	}

	defer func() {
		if retErr != nil {
			log.Infof(ctx, "runSandbox: deleting container ID from idIndex for sandbox %s", sbox.ID())
			if err2 := s.CtrIDIndex().Delete(sbox.ID()); err2 != nil {
				log.Warnf(ctx, "couldn't delete ctr id %s from idIndex", sbox.ID())
			}
		}
	}()

	// set log path inside log directory
	logPath := filepath.Join(logDir, sbox.ID()+".log")

	// Handle https://issues.k8s.io/44043
	if err := utils.EnsureSaneLogPath(logPath); err != nil {
		return nil, err
	}

	hostNetwork := securityContext.GetNamespaceOptions().GetNetwork() == pb.NamespaceMode_NODE

	hostname, err := getHostname(sbox.ID(), sbox.Config().Hostname, hostNetwork)
	if err != nil {
		return nil, err
	}
	g.SetHostname(hostname)

	// validate the runtime handler
	runtimeHandler, err := s.runtimeHandler(req)
	if err != nil {
		return nil, err
	}

	g.AddAnnotation(annotations.Metadata, string(metadataJSON))
	g.AddAnnotation(annotations.Labels, string(labelsJSON))
	g.AddAnnotation(annotations.Annotations, string(kubeAnnotationsJSON))
	g.AddAnnotation(annotations.LogPath, logPath)
	g.AddAnnotation(annotations.Name, sbox.Name())
	g.AddAnnotation(annotations.Namespace, namespace)
	g.AddAnnotation(annotations.ContainerType, annotations.ContainerTypeSandbox)
	g.AddAnnotation(annotations.SandboxID, sbox.ID())
	g.AddAnnotation(annotations.Image, s.config.PauseImage)
	g.AddAnnotation(annotations.ContainerName, containerName)
	g.AddAnnotation(annotations.ContainerID, sbox.ID())
	g.AddAnnotation(annotations.ShmPath, shmPath)
	g.AddAnnotation(annotations.PrivilegedRuntime, fmt.Sprintf("%v", privileged))
	g.AddAnnotation(annotations.RuntimeHandler, runtimeHandler)
	g.AddAnnotation(annotations.ResolvPath, resolvPath)
	g.AddAnnotation(annotations.HostName, hostname)
	g.AddAnnotation(annotations.NamespaceOptions, string(nsOptsJSON))
	g.AddAnnotation(annotations.KubeName, kubeName)
	g.AddAnnotation(annotations.HostNetwork, fmt.Sprintf("%v", hostNetwork))
	g.AddAnnotation(annotations.ContainerManager, lib.ContainerManagerCRIO)
	if podContainer.Config.Config.StopSignal != "" {
		// this key is defined in image-spec conversion document at https://github.com/opencontainers/image-spec/pull/492/files#diff-8aafbe2c3690162540381b8cdb157112R57
		g.AddAnnotation("org.opencontainers.image.stopSignal", podContainer.Config.Config.StopSignal)
	}

	if s.config.CgroupManager().IsSystemd() && node.SystemdHasCollectMode() {
		g.AddAnnotation("org.systemd.property.CollectMode", "'inactive-or-failed'")
	}

	created := time.Now()
	g.AddAnnotation(annotations.Created, created.Format(time.RFC3339Nano))

	portMappings := convertPortMappings(sbox.Config().GetPortMappings())
	portMappingsJSON, err := json.Marshal(portMappings)
	if err != nil {
		return nil, err
	}
	g.AddAnnotation(annotations.PortMappings, string(portMappingsJSON))

	cgroupParent, cgroupPath, err := s.config.CgroupManager().SandboxCgroupPath(sbox.Config().GetLinux().GetCgroupParent(), sbox.ID())
	if err != nil {
		return nil, err
	}
	if cgroupPath != "" {
		g.SetLinuxCgroupsPath(cgroupPath)
	}
	g.AddAnnotation(annotations.CgroupParent, cgroupParent)

	if sandboxIDMappings != nil {
		if err := g.AddOrReplaceLinuxNamespace(string(spec.UserNamespace), ""); err != nil {
			return nil, errors.Wrap(err, "add or replace linux namespace")
		}
		for _, uidmap := range sandboxIDMappings.UIDs() {
			g.AddLinuxUIDMapping(uint32(uidmap.HostID), uint32(uidmap.ContainerID), uint32(uidmap.Size))
		}
		for _, gidmap := range sandboxIDMappings.GIDs() {
			g.AddLinuxGIDMapping(uint32(gidmap.HostID), uint32(gidmap.ContainerID), uint32(gidmap.Size))
		}
	}

	sb, err := libsandbox.New(sbox.ID(), namespace, sbox.Name(), kubeName, logDir, labels, kubeAnnotations, processLabel, mountLabel, metadata, shmPath, cgroupParent, privileged, runtimeHandler, resolvPath, hostname, portMappings, hostNetwork, created, usernsMode)
	if err != nil {
		return nil, err
	}

	if err := s.addSandbox(sb); err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			log.Infof(ctx, "runSandbox: removing pod sandbox %s", sbox.ID())
			if err := s.removeSandbox(sbox.ID()); err != nil {
				log.Warnf(ctx, "could not remove pod sandbox: %v", err)
			}
		}
	}()

	if err := s.PodIDIndex().Add(sbox.ID()); err != nil {
		return nil, err
	}

	defer func() {
		if retErr != nil {
			log.Infof(ctx, "runSandbox: deleting pod ID %s from idIndex", sbox.ID())
			if err := s.PodIDIndex().Delete(sbox.ID()); err != nil {
				log.Warnf(ctx, "couldn't delete pod id %s from idIndex", sbox.ID())
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
	s.configureGeneratorForSysctls(ctx, g, hostNetwork, hostIPC)
	// extract linux sysctls from annotations and pass down to oci runtime
	// Will override any duplicate default systcl from crio.conf
	for key, value := range sbox.Config().GetLinux().GetSysctls() {
		g.AddLinuxSysctl(key, value)
	}

	// Set OOM score adjust of the infra container to be very low
	// so it doesn't get killed.
	g.SetProcessOOMScoreAdj(PodInfraOOMAdj)

	g.SetLinuxResourcesCPUShares(PodInfraCPUshares)

	// set up namespaces
	cleanupFuncs, err := s.configureGeneratorForSandboxNamespaces(hostNetwork, hostIPC, hostPID, sandboxIDMappings, sb, g)
	// We want to cleanup after ourselves if we are managing any namespaces and fail in this function.
	defer func() {
		if retErr != nil {
			log.Infof(ctx, "runSandbox: cleaning up namespaces after failing to run sandbox %s", sbox.ID())
			for idx := range cleanupFuncs {
				if err2 := cleanupFuncs[idx](); err2 != nil {
					log.Infof(ctx, "runSandbox: failed to cleanup namespace: %s", err2.Error())
				}
			}
		}
	}()
	if err != nil {
		return nil, err
	}

	saveOptions := generate.ExportOptions{}
	mountPoint, err := s.StorageRuntimeServer().StartContainer(sbox.ID())
	if err != nil {
		return nil, fmt.Errorf("failed to mount container %s in pod sandbox %s(%s): %v", containerName, sb.Name(), sbox.ID(), err)
	}
	defer func() {
		if retErr != nil {
			log.Infof(ctx, "runSandbox: stopping storage container for sandbox %s", sbox.ID())
			if err2 := s.StorageRuntimeServer().StopContainer(sbox.ID()); err2 != nil {
				log.Warnf(ctx, "couldn't stop storage container: %v: %v", sbox.ID(), err2)
			}
		}
	}()
	g.AddAnnotation(annotations.MountPoint, mountPoint)

	hostnamePath := fmt.Sprintf("%s/hostname", podContainer.RunDir)
	if err := ioutil.WriteFile(hostnamePath, []byte(hostname+"\n"), 0o644); err != nil {
		return nil, err
	}
	if err := label.Relabel(hostnamePath, mountLabel, false); err != nil && !errors.Is(err, unix.ENOTSUP) {
		return nil, err
	}
	mnt = spec.Mount{
		Type:        "bind",
		Source:      hostnamePath,
		Destination: "/etc/hostname",
		Options:     []string{"ro", "bind", "nodev", "nosuid", "noexec"},
	}
	pathsToChown = append(pathsToChown, hostnamePath, mountPoint)
	g.AddMount(mnt)
	g.AddAnnotation(annotations.HostnamePath, hostnamePath)
	sb.AddHostnamePath(hostnamePath)

	container, err := oci.NewContainer(sbox.ID(), containerName, podContainer.RunDir, logPath, labels, g.Config.Annotations, kubeAnnotations, s.config.PauseImage, "", "", nil, sbox.ID(), false, false, false, runtimeHandler, podContainer.Dir, created, podContainer.Config.Config.StopSignal)
	if err != nil {
		return nil, err
	}

	runtimeType, err := s.Runtime().ContainerRuntimeType(container)
	if err != nil {
		return nil, err
	}
	// If using kata runtime, the process label should be set to container_kvm_t
	// Keep in mind that kata does *not* apply any process label to containers within the VM
	// Note: the requirement here is that the name used for the runtime class has "kata" in it
	// or the runtime_type is set to "vm"
	if runtimeType == libconfig.RuntimeTypeVM || strings.Contains(strings.ToLower(runtimeHandler), "kata") {
		processLabel, err = selinux.KVMLabel(processLabel)
		if err != nil {
			return nil, err
		}
		g.SetProcessSelinuxLabel(processLabel)
	}

	container.SetMountPoint(mountPoint)

	container.SetIDMappings(sandboxIDMappings)

	if sandboxIDMappings != nil {
		if securityContext.GetNamespaceOptions().GetIpc() == pb.NamespaceMode_NODE {
			g.RemoveMount("/dev/mqueue")
			mqueue := spec.Mount{
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
			proc := spec.Mount{
				Type:        "bind",
				Source:      "/proc",
				Destination: "/proc",
				Options:     []string{"rw", "rbind", "nodev", "nosuid", "noexec"},
			}
			g.AddMount(proc)
		}
	}
	g.SetRootPath(mountPoint)

	if os.Getenv(rootlessEnvName) != "" {
		makeOCIConfigurationRootless(&g)
	}

	container.SetSpec(g.Config)

	if err := sb.SetInfraContainer(container); err != nil {
		return nil, err
	}

	var ips []string
	var result cnitypes.Result

	if s.config.ManageNSLifecycle {
		ips, result, err = s.networkStart(ctx, sb)
		if err != nil {
			return nil, err
		}
		if result != nil {
			resultCurrent, err := current.NewResultFromResult(result)
			if err != nil {
				return nil, err
			}
			cniResultJSON, err := json.Marshal(resultCurrent)
			if err != nil {
				return nil, err
			}
			g.AddAnnotation(annotations.CNIResult, string(cniResultJSON))
		}
		defer func() {
			if retErr != nil {
				log.Infof(ctx, "runSandbox: in manageNSLifecycle, stopping network for sandbox %s", sb.ID())
				if err2 := s.networkStop(ctx, sb); err2 != nil {
					log.Errorf(ctx, "error stopping network on cleanup: %v", err2)
				}
			}
		}()
	}

	for idx, ip := range ips {
		g.AddAnnotation(fmt.Sprintf("%s.%d", annotations.IP, idx), ip)
	}
	sb.AddIPs(ips)
	sb.SetNamespaceOptions(securityContext.GetNamespaceOptions())

	spp := securityContext.GetSeccompProfilePath()
	g.AddAnnotation(annotations.SeccompProfilePath, spp)
	sb.SetSeccompProfilePath(spp)
	if !privileged {
		if err := s.setupSeccomp(ctx, &g, spp); err != nil {
			return nil, err
		}
	}

	err = g.SaveToFile(filepath.Join(podContainer.Dir, "config.json"), saveOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to save template configuration for pod sandbox %s(%s): %v", sb.Name(), sbox.ID(), err)
	}
	if err = g.SaveToFile(filepath.Join(podContainer.RunDir, "config.json"), saveOptions); err != nil {
		return nil, fmt.Errorf("failed to write runtime configuration for pod sandbox %s(%s): %v", sb.Name(), sbox.ID(), err)
	}

	s.addInfraContainer(container)
	defer func() {
		if retErr != nil {
			log.Infof(ctx, "runSandbox: removing infra container %s", container.ID())
			s.removeInfraContainer(container)
		}
	}()

	if sandboxIDMappings != nil {
		rootPair := sandboxIDMappings.RootPair()
		for _, path := range pathsToChown {
			if err := makeAccessible(path, rootPair.UID, rootPair.GID, true); err != nil {
				return nil, errors.Wrapf(err, "cannot chown %s to %d:%d", path, rootPair.UID, rootPair.GID)
			}
		}
		if err := makeMountsAccessible(rootPair.UID, rootPair.GID, g.Config.Mounts); err != nil {
			return nil, err
		}
	}

	if err := s.createContainerPlatform(container, sb.CgroupParent(), sandboxIDMappings); err != nil {
		return nil, err
	}

	if err := s.Runtime().StartContainer(container); err != nil {
		return nil, err
	}

	defer func() {
		if retErr != nil {
			// Clean-up steps from RemovePodSanbox
			log.Infof(ctx, "runSandbox: stopping container %s", container.ID())
			if err2 := s.Runtime().StopContainer(ctx, container, int64(10)); err2 != nil {
				log.Warnf(ctx, "failed to stop container %s: %v", container.Name(), err2)
			}
			if err2 := s.Runtime().WaitContainerStateStopped(ctx, container); err2 != nil {
				log.Warnf(ctx, "failed to get container 'stopped' status %s in pod sandbox %s: %v", container.Name(), sb.ID(), err2)
			}
			log.Infof(ctx, "runSandbox: deleting container %s", container.ID())
			if err2 := s.Runtime().DeleteContainer(container); err2 != nil {
				log.Warnf(ctx, "failed to delete container %s in pod sandbox %s: %v", container.Name(), sb.ID(), err2)
			}
			log.Infof(ctx, "runSandbox: writing container %s state to disk", container.ID())
			if err2 := s.ContainerStateToDisk(container); err2 != nil {
				log.Warnf(ctx, "failed to write container state %s in pod sandbox %s: %v", container.Name(), sb.ID(), err2)
			}
		}
	}()

	if err := s.ContainerStateToDisk(container); err != nil {
		log.Warnf(ctx, "unable to write containers %s state to disk: %v", container.ID(), err)
	}

	if !s.config.ManageNSLifecycle {
		ips, _, err = s.networkStart(ctx, sb)
		if err != nil {
			return nil, err
		}
		defer func() {
			if retErr != nil {
				log.Infof(ctx, "runSandbox: in not manageNSLifecycle, stopping network for sandbox %s", sb.ID())
				if err2 := s.networkStop(ctx, sb); err2 != nil {
					log.Errorf(ctx, "error stopping network on cleanup: %v", err2)
				}
			}
		}()
	} else if err := sb.CreateManagedPidNamespace(&s.config); err != nil {
		// now the infra container has started, we can pin the pid namespace
		// to ease container's access of it later
		return nil, err
	}
	sb.AddIPs(ips)

	sb.SetCreated()

	if ctx.Err() == context.Canceled || ctx.Err() == context.DeadlineExceeded {
		log.Infof(ctx, "runSandbox: context was either canceled or the deadline was exceeded: %v", ctx.Err())
		return nil, ctx.Err()
	}

	log.Infof(ctx, "Ran pod sandbox %s with infra container: %s", container.ID(), container.Description())
	resp = &pb.RunPodSandboxResponse{PodSandboxId: sbox.ID()}
	return resp, nil
}

func setupShm(podSandboxRunDir, mountLabel string) (shmPath string, _ error) {
	shmPath = filepath.Join(podSandboxRunDir, "shm")
	if err := os.Mkdir(shmPath, 0o700); err != nil {
		return "", err
	}
	shmOptions := "mode=1777,size=" + strconv.Itoa(libsandbox.DefaultShmSize)
	if err := unix.Mount("shm", shmPath, "tmpfs", unix.MS_NOEXEC|unix.MS_NOSUID|unix.MS_NODEV,
		label.FormatMountLabel(shmOptions, mountLabel)); err != nil {
		return "", fmt.Errorf("failed to mount shm tmpfs for pod: %v", err)
	}
	return shmPath, nil
}

// PauseCommand returns the pause command for the provided image configuration.
func PauseCommand(cfg *libconfig.Config, image *v1.Image) ([]string, error) {
	if cfg == nil {
		return nil, fmt.Errorf("provided configuration is nil")
	}

	// This has been explicitly set by the user, since the configuration
	// default is `/pause`
	if cfg.PauseCommand == "" {
		if image == nil ||
			(len(image.Config.Entrypoint) == 0 && len(image.Config.Cmd) == 0) {
			return nil, fmt.Errorf(
				"unable to run pause image %q: %s",
				cfg.PauseImage,
				"neither Cmd nor Entrypoint specified",
			)
		}
		cmd := []string{}
		cmd = append(cmd, image.Config.Entrypoint...)
		cmd = append(cmd, image.Config.Cmd...)
		return cmd, nil
	}
	return []string{cfg.PauseCommand}, nil
}

func (s *Server) configureGeneratorForSysctls(ctx context.Context, g generate.Generator, hostNetwork, hostIPC bool) {
	sysctls, err := s.config.RuntimeConfig.Sysctls()
	if err != nil {
		log.Warnf(ctx, "sysctls invalid: %v", err)
	}

	for _, sysctl := range sysctls {
		if err := sysctl.Validate(hostNetwork, hostIPC); err != nil {
			log.Warnf(ctx, "skipping invalid sysctl %s: %v", sysctl, err)
			continue
		}
		g.AddLinuxSysctl(sysctl.Key(), sysctl.Value())
	}
}

// configureGeneratorForSandboxNamespaces set the linux namespaces for the generator, based on whether the pod is sharing namespaces with the host,
// as well as whether CRI-O should be managing the namespace lifecycle.
// it returns a slice of cleanup funcs, all of which are the respective NamespaceRemove() for the sandbox.
// The caller should defer the cleanup funcs if there is an error, to make sure each namespace we are managing is properly cleaned up.
func (s *Server) configureGeneratorForSandboxNamespaces(hostNetwork, hostIPC, hostPID bool, idMappings *idtools.IDMappings, sb *libsandbox.Sandbox, g generate.Generator) (cleanupFuncs []func() error, retErr error) {
	managedNamespaces := make([]libsandbox.NSType, 0, 3)
	if hostNetwork {
		if err := g.RemoveLinuxNamespace(string(spec.NetworkNamespace)); err != nil {
			return nil, err
		}
	} else if s.config.ManageNSLifecycle {
		managedNamespaces = append(managedNamespaces, libsandbox.NETNS)
	}

	if hostIPC {
		if err := g.RemoveLinuxNamespace(string(spec.IPCNamespace)); err != nil {
			return nil, err
		}
	} else if s.config.ManageNSLifecycle {
		managedNamespaces = append(managedNamespaces, libsandbox.IPCNS)
	}

	if idMappings == nil {
		if err := g.RemoveLinuxNamespace(string(spec.UserNamespace)); err != nil {
			return nil, err
		}
	} else if s.config.ManageNSLifecycle {
		managedNamespaces = append(managedNamespaces, libsandbox.USERNS)
	}

	// Since we need a process to hold open the PID namespace, CRI-O can't manage the NS lifecycle
	if hostPID {
		if err := g.RemoveLinuxNamespace(string(spec.PIDNamespace)); err != nil {
			return nil, err
		}
	}

	// There's no option to set hostUTS
	if s.config.ManageNSLifecycle {
		managedNamespaces = append(managedNamespaces, libsandbox.UTSNS)

		// now that we've configured the namespaces we're sharing, tell sandbox to configure them
		managedNamespaces, err := sb.CreateManagedNamespaces(managedNamespaces, idMappings, &s.config)
		if err != nil {
			return nil, err
		}

		cleanupFuncs = append(cleanupFuncs, sb.RemoveManagedNamespaces)

		if err := configureGeneratorGivenNamespacePaths(managedNamespaces, g); err != nil {
			return cleanupFuncs, err
		}
	}

	return cleanupFuncs, nil
}

// configureGeneratorGivenNamespacePaths takes a map of nsType -> nsPath. It configures the generator
// to add or replace the defaults to these paths
func configureGeneratorGivenNamespacePaths(managedNamespaces []*libsandbox.ManagedNamespace, g generate.Generator) error {
	typeToSpec := map[libsandbox.NSType]spec.LinuxNamespaceType{
		libsandbox.IPCNS:  spec.IPCNamespace,
		libsandbox.NETNS:  spec.NetworkNamespace,
		libsandbox.UTSNS:  spec.UTSNamespace,
		libsandbox.USERNS: spec.UserNamespace,
	}

	for _, ns := range managedNamespaces {
		// allow for empty paths, as this namespace just shouldn't be configured
		if ns.Path() == "" {
			continue
		}
		nsForSpec := typeToSpec[ns.Type()]
		if nsForSpec == "" {
			return errors.Errorf("Invalid namespace type %s", nsForSpec)
		}
		err := g.AddOrReplaceLinuxNamespace(string(nsForSpec), ns.Path())
		if err != nil {
			return err
		}
	}
	return nil
}
