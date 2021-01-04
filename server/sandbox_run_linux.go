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
	"github.com/containers/podman/v3/pkg/annotations"
	"github.com/containers/podman/v3/pkg/rootless"
	selinux "github.com/containers/podman/v3/pkg/selinux"
	"github.com/containers/storage"
	"github.com/containers/storage/pkg/idtools"
	"github.com/cri-o/cri-o/internal/config/node"
	"github.com/cri-o/cri-o/internal/config/nsmgr"
	"github.com/cri-o/cri-o/internal/lib"
	libsandbox "github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/log"
	oci "github.com/cri-o/cri-o/internal/oci"
	"github.com/cri-o/cri-o/internal/resourcestore"
	ann "github.com/cri-o/cri-o/pkg/annotations"
	libconfig "github.com/cri-o/cri-o/pkg/config"
	"github.com/cri-o/cri-o/pkg/sandbox"
	"github.com/cri-o/cri-o/server/cri/types"
	"github.com/cri-o/cri-o/utils"
	json "github.com/json-iterator/go"
	spec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/opencontainers/selinux/go-selinux/label"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/api/resource"
	kubeletTypes "k8s.io/kubernetes/pkg/kubelet/types"
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

func (s *Server) configureSandboxIDMappings(mode string, sc *types.LinuxSandboxSecurityContext) (*storage.IDMappingOptions, error) {
	if mode == "" {
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
		user := sc.RunAsUser
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
		if sc.RunAsUser != nil {
			if keepID || mapToRoot {
				id := 0
				if keepID {
					id = int(sc.RunAsUser.Value)
				}
				ret.AutoUserNsOpts.AdditionalUIDMappings = append(
					ret.AutoUserNsOpts.AdditionalUIDMappings,
					idtools.IDMap{
						ContainerID: id,
						HostID:      int(sc.RunAsUser.Value),
						Size:        1,
					})
			} else {
				m := addToMappingsIfMissing(ret.AutoUserNsOpts.AdditionalUIDMappings, sc.RunAsUser.Value)
				ret.AutoUserNsOpts.AdditionalUIDMappings = m
			}
		}
		if sc.RunAsGroup != nil {
			if keepID || mapToRoot {
				id := 0
				if keepID {
					id = int(sc.RunAsGroup.Value)
				}
				ret.AutoUserNsOpts.AdditionalGIDMappings = append(
					ret.AutoUserNsOpts.AdditionalGIDMappings,
					idtools.IDMap{
						ContainerID: id,
						HostID:      int(sc.RunAsGroup.Value),
						Size:        1,
					})
			} else {
				m := addToMappingsIfMissing(ret.AutoUserNsOpts.AdditionalGIDMappings, sc.RunAsGroup.Value)
				ret.AutoUserNsOpts.AdditionalGIDMappings = m
			}
		}
		for _, g := range sc.SupplementalGroups {
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
		if sc.RunAsUser != nil {
			uids = addToMappingsIfMissing(uids, sc.RunAsUser.Value)
		}
		if sc.RunAsGroup != nil {
			gids = addToMappingsIfMissing(gids, sc.RunAsGroup.Value)
		}
		for _, g := range sc.SupplementalGroups {
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

func (s *Server) runPodSandbox(ctx context.Context, req *types.RunPodSandboxRequest) (resp *types.RunPodSandboxResponse, retErr error) {
	s.updateLock.RLock()
	defer s.updateLock.RUnlock()

	sbox := sandbox.New()
	if err := sbox.SetConfig(req.Config); err != nil {
		return nil, errors.Wrap(err, "setting sandbox config")
	}

	pathsToChown := []string{}

	// we need to fill in the container name, as it is not present in the request. Luckily, it is a constant.
	log.Infof(ctx, "Running pod sandbox: %s%s", translateLabelsToDescription(sbox.Config().Labels), types.InfraContainerName)

	kubeName := sbox.Config().Metadata.Name
	namespace := sbox.Config().Metadata.Namespace
	attempt := sbox.Config().Metadata.Attempt

	if err := sbox.SetNameAndID(); err != nil {
		return nil, errors.Wrap(err, "setting pod sandbox name and id")
	}

	resourceCleaner := resourcestore.NewResourceCleaner()
	defer func() {
		// no error, no need to cleanup
		if retErr == nil || isContextError(retErr) {
			return
		}
		if err := resourceCleaner.Cleanup(); err != nil {
			log.Errorf(ctx, "Unable to cleanup: %v", err)
		}
	}()

	if _, err := s.ReservePodName(sbox.ID(), sbox.Name()); err != nil {
		cachedID, resourceErr := s.getResourceOrWait(ctx, sbox.Name(), "sandbox")
		if resourceErr == nil {
			return &types.RunPodSandboxResponse{PodSandboxID: cachedID}, nil
		}
		return nil, errors.Wrapf(err, resourceErr.Error())
	}

	description := fmt.Sprintf("runSandbox: releasing pod sandbox name: %s", sbox.Name())
	resourceCleaner.Add(ctx, description, func() error {
		log.Infof(ctx, description)
		s.ReleasePodName(sbox.Name())
		return nil
	})

	// validate the runtime handler
	runtimeHandler, err := s.runtimeHandler(req)
	if err != nil {
		return nil, err
	}

	if err := s.Runtime().FilterDisallowedAnnotations(runtimeHandler, sbox.Config().Annotations); err != nil {
		return nil, errors.Wrap(err, "filter disallowed annotations")
	}

	kubeAnnotations := sbox.Config().Annotations

	usernsMode := kubeAnnotations[ann.UsernsModeAnnotation]

	idMappingsOptions, err := s.configureSandboxIDMappings(usernsMode, sbox.Config().Linux.SecurityContext)
	if err != nil {
		return nil, err
	}

	containerName, err := s.ReserveSandboxContainerIDAndName(sbox.Config())
	if err != nil {
		return nil, err
	}
	description = fmt.Sprintf("runSandbox: releasing container name: %s", containerName)
	resourceCleaner.Add(ctx, description, func() error {
		log.Infof(ctx, description)
		s.ReleaseContainerName(containerName)
		return nil
	})

	var labelOptions []string
	securityContext := sbox.Config().Linux.SecurityContext
	selinuxConfig := securityContext.SelinuxOptions
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
		sbox.Config().Metadata.UID,
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
	description = fmt.Sprintf("runSandbox: removing pod sandbox from storage: %s", sbox.ID())
	resourceCleaner.Add(ctx, description, func() error {
		log.Infof(ctx, description)
		err2 := s.StorageRuntimeServer().DeleteContainer(sbox.ID())
		if err2 != nil {
			log.Warnf(ctx, "Could not cleanup pod sandbox %q: %v", sbox.ID(), err2)
		}
		return err2
	})

	// set log directory
	logDir := sbox.Config().LogDirectory
	if logDir == "" {
		logDir = filepath.Join(s.config.LogDir, sbox.ID())
	}
	// This should always be absolute from k8s.
	if !filepath.IsAbs(logDir) {
		return nil, fmt.Errorf("requested logDir for sbox ID %s is a relative path: %s", sbox.ID(), logDir)
	}
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return nil, err
	}

	var sandboxIDMappings *idtools.IDMappings
	if idMappingsOptions != nil {
		sandboxIDMappings = idtools.NewIDMappingsFromMaps(idMappingsOptions.UIDMap, idMappingsOptions.GIDMap)
	}

	// TODO: factor generating/updating the spec into something other projects can vendor
	if err := sbox.InitInfraContainer(&s.config, &podContainer); err != nil {
		return nil, err
	}
	g := sbox.Spec()
	pathsToChown = append(pathsToChown, sbox.ResolvPath())

	// add metadata
	metadata := sbox.Config().Metadata
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}

	// add labels
	labels := sbox.Config().Labels

	if err := validateLabels(labels); err != nil {
		return nil, err
	}

	// Add special container name label for the infra container
	if labels != nil {
		labels[kubeletTypes.KubernetesContainerNameLabel] = types.InfraContainerName
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
	capabilities := &types.Capability{}
	g.ClearProcessCapabilities()
	if err := setupCapabilities(g, capabilities, s.config.DefaultCapabilities); err != nil {
		return nil, err
	}

	nsOptsJSON, err := json.Marshal(securityContext.NamespaceOptions)
	if err != nil {
		return nil, err
	}

	hostIPC := securityContext.NamespaceOptions.Ipc == types.NamespaceModeNODE
	hostPID := securityContext.NamespaceOptions.Pid == types.NamespaceModeNODE

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
		shmSize := int64(libsandbox.DefaultShmSize)
		if shmSizeStr, ok := kubeAnnotations[ann.ShmSizeAnnotation]; ok {
			quantity, err := resource.ParseQuantity(shmSizeStr)
			if err != nil {
				return nil, fmt.Errorf("failed to parse shm size '%s': %v", shmSizeStr, err)
			}
			shmSize = quantity.Value()
		}
		shmPath, err = setupShm(podContainer.RunDir, mountLabel, shmSize)
		if err != nil {
			return nil, err
		}
		pathsToChown = append(pathsToChown, shmPath)
		description = fmt.Sprintf("runSandbox: unmounting shmPath for sandbox %s", sbox.ID())
		resourceCleaner.Add(ctx, description, func() error {
			log.Infof(ctx, description)
			err2 := unix.Unmount(shmPath, unix.MNT_DETACH)
			if err2 != nil {
				log.Warnf(ctx, "Failed to unmount shm for sandbox: %v", err2)
			}
			return err2
		})
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

	description = fmt.Sprintf("runSandbox: deleting container ID from idIndex for sandbox %s", sbox.ID())
	resourceCleaner.Add(ctx, description, func() error {
		log.Infof(ctx, description)
		err2 := s.CtrIDIndex().Delete(sbox.ID())
		if err2 != nil {
			log.Warnf(ctx, "Could not delete ctr id %s from idIndex", sbox.ID())
		}
		return err2
	})

	// set log path inside log directory
	logPath := filepath.Join(logDir, sbox.ID()+".log")

	// Handle https://issues.k8s.io/44043
	if err := utils.EnsureSaneLogPath(logPath); err != nil {
		return nil, err
	}

	hostNetwork := securityContext.NamespaceOptions.Network == types.NamespaceModeNODE

	hostname, err := getHostname(sbox.ID(), sbox.Config().Hostname, hostNetwork)
	if err != nil {
		return nil, err
	}
	g.SetHostname(hostname)

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
	g.AddAnnotation(annotations.ResolvPath, sbox.ResolvPath())
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

	portMappings := convertPortMappings(sbox.Config().PortMappings)
	portMappingsJSON, err := json.Marshal(portMappings)
	if err != nil {
		return nil, err
	}
	g.AddAnnotation(annotations.PortMappings, string(portMappingsJSON))

	cgroupParent, cgroupPath, err := s.config.CgroupManager().SandboxCgroupPath(sbox.Config().Linux.CgroupParent, sbox.ID())
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

	sbMetadata := &libsandbox.Metadata{
		Name:      metadata.Name,
		UID:       metadata.UID,
		Namespace: metadata.Namespace,
		Attempt:   metadata.Attempt,
	}
	sb, err := libsandbox.New(sbox.ID(), namespace, sbox.Name(), kubeName, logDir, labels, kubeAnnotations, processLabel, mountLabel, sbMetadata, shmPath, cgroupParent, privileged, runtimeHandler, sbox.ResolvPath(), hostname, portMappings, hostNetwork, created, usernsMode)
	if err != nil {
		return nil, err
	}

	sb.SetDNSConfig(sbox.Config().DNSConfig)

	if err := s.addSandbox(sb); err != nil {
		return nil, err
	}
	description = fmt.Sprintf("runSandbox: removing pod sandbox %s", sbox.ID())
	resourceCleaner.Add(ctx, description, func() error {
		log.Infof(ctx, description)
		err := s.removeSandbox(sbox.ID())
		if err != nil {
			log.Warnf(ctx, "Could not remove pod sandbox: %v", err)
		}
		return err
	})

	if err := s.PodIDIndex().Add(sbox.ID()); err != nil {
		return nil, err
	}

	description = fmt.Sprintf("runSandbox: deleting pod ID %s from idIndex", sbox.ID())
	resourceCleaner.Add(ctx, description, func() error {
		log.Infof(ctx, description)
		err := s.PodIDIndex().Delete(sbox.ID())
		if err != nil {
			log.Warnf(ctx, "Could not delete pod id %s from idIndex", sbox.ID())
		}
		return err
	})

	for k, v := range kubeAnnotations {
		g.AddAnnotation(k, v)
	}
	for k, v := range labels {
		g.AddAnnotation(k, v)
	}

	// Add default sysctls given in crio.conf
	sysctls := s.configureGeneratorForSysctls(ctx, g, hostNetwork, hostIPC, req.Config.Linux.Sysctls)

	// set up namespaces
	nsCleanupFuncs, err := s.configureGeneratorForSandboxNamespaces(hostNetwork, hostIPC, hostPID, sandboxIDMappings, sysctls, sb, g)
	// We want to cleanup after ourselves if we are managing any namespaces and fail in this function.
	nsCleanupDescription := fmt.Sprintf("runSandbox: cleaning up namespaces after failing to run sandbox %s", sbox.ID())
	nsCleanupFunc := func() error {
		log.Infof(ctx, description)
		for idx := range nsCleanupFuncs {
			if err2 := nsCleanupFuncs[idx](); err2 != nil {
				log.Infof(ctx, "RunSandbox: failed to cleanup namespace: %s", err2.Error())
				return err2
			}
		}
		return nil
	}
	if err != nil {
		resourceCleaner.Add(ctx, nsCleanupDescription, nsCleanupFunc)
		return nil, err
	}

	// now that we have the namespaces, we should create the network if we're managing namespace Lifecycle
	var ips []string
	var result cnitypes.Result

	ips, result, err = s.networkStart(ctx, sb)
	if err != nil {
		resourceCleaner.Add(ctx, nsCleanupDescription, nsCleanupFunc)
		return nil, err
	}
	description = fmt.Sprintf("runSandbox: stopping network for sandbox %s", sb.ID())
	resourceCleaner.Add(ctx, description, func() error {
		log.Infof(ctx, description)
		// use a new context to prevent an expired context from preventing a stop
		if err := s.networkStop(context.Background(), sb); err != nil {
			log.Errorf(ctx, "Error stopping network on cleanup: %v", err)
			return err
		}

		// Now that we've succeeded in stopping the network, cleanup namespaces
		log.Infof(ctx, nsCleanupDescription)
		return nsCleanupFunc()
	})
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

	// Set OOM score adjust of the infra container to be very low
	// so it doesn't get killed.
	g.SetProcessOOMScoreAdj(PodInfraOOMAdj)

	g.SetLinuxResourcesCPUShares(PodInfraCPUshares)

	// When infra-ctr-cpuset specified, set the infra container CPU set
	if s.config.InfraCtrCPUSet != "" {
		log.Debugf(ctx, "Set the infra container cpuset to %q", s.config.InfraCtrCPUSet)
		g.SetLinuxResourcesCPUCpus(s.config.InfraCtrCPUSet)
	}

	saveOptions := generate.ExportOptions{}
	mountPoint, err := s.StorageRuntimeServer().StartContainer(sbox.ID())
	if err != nil {
		return nil, fmt.Errorf("failed to mount container %s in pod sandbox %s(%s): %v", containerName, sb.Name(), sbox.ID(), err)
	}
	description = fmt.Sprintf("runSandbox: stopping storage container for sandbox %s", sbox.ID())
	resourceCleaner.Add(ctx, description, func() error {
		log.Infof(ctx, description)
		err2 := s.StorageRuntimeServer().StopContainer(sbox.ID())
		if err2 != nil {
			log.Warnf(ctx, "Could not stop storage container: %v: %v", sbox.ID(), err2)
		}
		return err2
	})
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

	if sandboxIDMappings != nil {
		if securityContext.NamespaceOptions.Ipc == types.NamespaceModeNODE {
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
		if securityContext.NamespaceOptions.Pid == types.NamespaceModeNODE {
			g.RemoveMount("/proc")
			proc := spec.Mount{
				Type:        "bind",
				Source:      "/proc",
				Destination: "/proc",
				Options:     []string{"rw", "rbind", "nodev", "nosuid", "noexec"},
			}
			g.AddMount(proc)
		}
		rootPair := sandboxIDMappings.RootPair()
		for _, path := range pathsToChown {
			if err := os.Chown(path, rootPair.UID, rootPair.GID); err != nil {
				return nil, errors.Wrapf(err, "cannot chown %s to %d:%d", path, rootPair.UID, rootPair.GID)
			}
		}
	}
	g.SetRootPath(mountPoint)

	if os.Getenv(rootlessEnvName) != "" {
		makeOCIConfigurationRootless(g)
	}

	sb.SetNamespaceOptions(securityContext.NamespaceOptions)

	seccompProfilePath := securityContext.SeccompProfilePath
	g.AddAnnotation(annotations.SeccompProfilePath, seccompProfilePath)
	sb.SetSeccompProfilePath(seccompProfilePath)
	if !privileged {
		if err := s.Config().Seccomp().Setup(
			ctx, g, securityContext.Seccomp, seccompProfilePath,
		); err != nil {
			return nil, errors.Wrap(err, "setup seccomp")
		}
	}

	runtimeType, err := s.Runtime().RuntimeType(runtimeHandler)
	if err != nil {
		return nil, err
	}

	// A container is kernel separated if we're using shimv2, or we're using a kata v1 binary
	podIsKernelSeparated := runtimeType == libconfig.RuntimeTypeVM ||
		strings.Contains(strings.ToLower(runtimeHandler), "kata") ||
		(runtimeHandler == "" && strings.Contains(strings.ToLower(s.config.DefaultRuntime), "kata"))

	var container *oci.Container
	// In the case of kernel separated containers, we need the infra container to create the VM for the pod
	if sb.NeedsInfra(s.config.DropInfraCtr) || podIsKernelSeparated {
		log.Debugf(ctx, "Keeping infra container for pod %s", sbox.ID())
		container, err = oci.NewContainer(sbox.ID(), containerName, podContainer.RunDir, logPath, labels, g.Config.Annotations, kubeAnnotations, s.config.PauseImage, "", "", nil, sbox.ID(), false, false, false, runtimeHandler, podContainer.Dir, created, podContainer.Config.Config.StopSignal)
		if err != nil {
			return nil, err
		}
		// If using a kernel separated container runtime, the process label should be set to container_kvm_t
		// Keep in mind that kata does *not* apply any process label to containers within the VM
		if podIsKernelSeparated {
			processLabel, err = selinux.KVMLabel(processLabel)
			if err != nil {
				return nil, err
			}
			g.SetProcessSelinuxLabel(processLabel)
		}

		container.SetMountPoint(mountPoint)

		container.SetSpec(g.Config)
	} else {
		log.Debugf(ctx, "Dropping infra container for pod %s", sbox.ID())
		container = oci.NewSpoofedContainer(sbox.ID(), containerName, labels, sbox.ID(), created, podContainer.RunDir)
		g.AddAnnotation(ann.SpoofedContainer, "true")
		if err := s.config.CgroupManager().CreateSandboxCgroup(cgroupParent, sbox.ID()); err != nil {
			return nil, errors.Wrapf(err, "create dropped infra %s cgroup", sbox.ID())
		}
	}
	// needed for getSandboxIDMappings()
	container.SetIDMappings(sandboxIDMappings)

	if err := sb.SetInfraContainer(container); err != nil {
		return nil, err
	}

	if err = g.SaveToFile(filepath.Join(podContainer.Dir, "config.json"), saveOptions); err != nil {
		return nil, fmt.Errorf("failed to save template configuration for pod sandbox %s(%s): %v", sb.Name(), sbox.ID(), err)
	}
	if err = g.SaveToFile(filepath.Join(podContainer.RunDir, "config.json"), saveOptions); err != nil {
		return nil, fmt.Errorf("failed to write runtime configuration for pod sandbox %s(%s): %v", sb.Name(), sbox.ID(), err)
	}

	s.addInfraContainer(container)
	description = fmt.Sprintf("runSandbox: removing infra container %s", container.ID())
	resourceCleaner.Add(ctx, description, func() error {
		log.Infof(ctx, description)
		s.removeInfraContainer(container)
		return nil
	})

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

	if err := s.createContainerPlatform(ctx, container, sb.CgroupParent(), sandboxIDMappings); err != nil {
		return nil, err
	}

	if err := s.Runtime().StartContainer(ctx, container); err != nil {
		return nil, err
	}

	description = fmt.Sprintf("runSandbox: stopping container %s", container.ID())
	resourceCleaner.Add(ctx, description, func() error {
		// Clean-up steps from RemovePodSanbox
		log.Infof(ctx, description)
		if err := s.ContainerServer.StopContainer(ctx, container, int64(10)); err != nil {
			return errors.Errorf("failed to stop container for removal")
		}

		log.Infof(ctx, "RunSandbox: deleting container %s", container.ID())
		if err2 := s.Runtime().DeleteContainer(ctx, container); err2 != nil {
			log.Warnf(ctx, "Failed to delete container %s in pod sandbox %s: %v", container.Name(), sb.ID(), err2)
			return err2
		}
		log.Infof(ctx, "RunSandbox: writing container %s state to disk", container.ID())
		if err2 := s.ContainerStateToDisk(ctx, container); err2 != nil {
			log.Warnf(ctx, "Failed to write container state %s in pod sandbox %s: %v", container.Name(), sb.ID(), err2)
			return err2
		}
		return nil
	})

	if err := s.ContainerStateToDisk(ctx, container); err != nil {
		log.Warnf(ctx, "Unable to write containers %s state to disk: %v", container.ID(), err)
	}

	for idx, ip := range ips {
		g.AddAnnotation(fmt.Sprintf("%s.%d", annotations.IP, idx), ip)
	}
	sb.AddIPs(ips)

	if isContextError(ctx.Err()) {
		if err := s.resourceStore.Put(sbox.Name(), sb, resourceCleaner); err != nil {
			log.Errorf(ctx, "RunSandbox: failed to save progress of sandbox %s: %v", sbox.ID(), err)
		}
		log.Infof(ctx, "RunSandbox: context was either canceled or the deadline was exceeded: %v", ctx.Err())
		return nil, ctx.Err()
	}
	sb.SetCreated()

	log.Infof(ctx, "Ran pod sandbox %s with infra container: %s", container.ID(), container.Description())
	resp = &types.RunPodSandboxResponse{PodSandboxID: sbox.ID()}
	return resp, nil
}

func setupShm(podSandboxRunDir, mountLabel string, shmSize int64) (shmPath string, _ error) {
	if shmSize <= 0 {
		return "", fmt.Errorf("shm size %d must be greater than 0", shmSize)
	}

	shmPath = filepath.Join(podSandboxRunDir, "shm")
	if err := os.Mkdir(shmPath, 0o700); err != nil {
		return "", err
	}
	shmOptions := "mode=1777,size=" + strconv.FormatInt(shmSize, 10)
	if err := unix.Mount("shm", shmPath, "tmpfs", unix.MS_NOEXEC|unix.MS_NOSUID|unix.MS_NODEV,
		label.FormatMountLabel(shmOptions, mountLabel)); err != nil {
		return "", fmt.Errorf("failed to mount shm tmpfs for pod: %v", err)
	}
	return shmPath, nil
}

func (s *Server) configureGeneratorForSysctls(ctx context.Context, g *generate.Generator, hostNetwork, hostIPC bool, sysctls map[string]string) map[string]string {
	sysctlsToReturn := make(map[string]string)
	defaultSysctls, err := s.config.RuntimeConfig.Sysctls()
	if err != nil {
		log.Warnf(ctx, "Sysctls invalid: %v", err)
	}

	for _, sysctl := range defaultSysctls {
		if err := sysctl.Validate(hostNetwork, hostIPC); err != nil {
			log.Warnf(ctx, "Skipping invalid sysctl %s: %v", sysctl, err)
			continue
		}
		g.AddLinuxSysctl(sysctl.Key(), sysctl.Value())
		sysctlsToReturn[sysctl.Key()] = sysctl.Value()
	}

	// extract linux sysctls from annotations and pass down to oci runtime
	// Will override any duplicate default systcl from crio.conf
	for key, value := range sysctls {
		g.AddLinuxSysctl(key, value)
		sysctlsToReturn[key] = value
	}
	return sysctlsToReturn
}

// configureGeneratorForSandboxNamespaces set the linux namespaces for the generator, based on whether the pod is sharing namespaces with the host,
// as well as whether CRI-O should be managing the namespace lifecycle.
// it returns a slice of cleanup funcs, all of which are the respective NamespaceRemove() for the sandbox.
// The caller should defer the cleanup funcs if there is an error, to make sure each namespace we are managing is properly cleaned up.
func (s *Server) configureGeneratorForSandboxNamespaces(hostNetwork, hostIPC, hostPID bool, idMappings *idtools.IDMappings, sysctls map[string]string, sb *libsandbox.Sandbox, g *generate.Generator) (cleanupFuncs []func() error, retErr error) {
	// Since we need a process to hold open the PID namespace, CRI-O can't manage the NS lifecycle
	if hostPID {
		if err := g.RemoveLinuxNamespace(string(spec.PIDNamespace)); err != nil {
			return nil, err
		}
	}
	namespaceConfig := &nsmgr.PodNamespacesConfig{
		Sysctls:    sysctls,
		IDMappings: idMappings,
		Namespaces: []*nsmgr.PodNamespaceConfig{
			{
				Type: nsmgr.IPCNS,
				Host: hostIPC,
			},
			{
				Type: nsmgr.NETNS,
				Host: hostNetwork,
			},
			{
				Type: nsmgr.UTSNS, // there is no option for host UTSNS
			},
		},
	}
	if idMappings != nil {
		namespaceConfig.Namespaces = append(namespaceConfig.Namespaces, &nsmgr.PodNamespaceConfig{
			Type: nsmgr.USERNS,
		})
	}

	// now that we've configured the namespaces we're sharing, create them
	namespaces, err := s.config.NamespaceManager().NewPodNamespaces(namespaceConfig)
	if err != nil {
		return nil, err
	}

	sb.AddManagedNamespaces(namespaces)

	cleanupFuncs = append(cleanupFuncs, sb.RemoveManagedNamespaces)

	if err := configureGeneratorGivenNamespacePaths(sb.NamespacePaths(), g); err != nil {
		return cleanupFuncs, err
	}

	return cleanupFuncs, nil
}

// configureGeneratorGivenNamespacePaths takes a map of nsType -> nsPath. It configures the generator
// to add or replace the defaults to these paths
func configureGeneratorGivenNamespacePaths(managedNamespaces []*libsandbox.ManagedNamespace, g *generate.Generator) error {
	typeToSpec := map[nsmgr.NSType]spec.LinuxNamespaceType{
		nsmgr.IPCNS:  spec.IPCNamespace,
		nsmgr.NETNS:  spec.NetworkNamespace,
		nsmgr.UTSNS:  spec.UTSNamespace,
		nsmgr.USERNS: spec.UserNamespace,
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
