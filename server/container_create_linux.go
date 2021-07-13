// +build linux

package server

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/containers/buildah/pkg/secrets"
	"github.com/containers/buildah/util"
	"github.com/containers/podman/v3/pkg/rootless"
	selinux "github.com/containers/podman/v3/pkg/selinux"
	cstorage "github.com/containers/storage"
	"github.com/containers/storage/pkg/idtools"
	"github.com/containers/storage/pkg/mount"
	"github.com/cri-o/cri-o/internal/config/cgmgr"
	"github.com/cri-o/cri-o/internal/config/device"
	"github.com/cri-o/cri-o/internal/config/node"
	"github.com/cri-o/cri-o/internal/config/rdt"
	"github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/log"
	oci "github.com/cri-o/cri-o/internal/oci"
	"github.com/cri-o/cri-o/internal/storage"
	crioann "github.com/cri-o/cri-o/pkg/annotations"
	ctrIface "github.com/cri-o/cri-o/pkg/container"
	"github.com/cri-o/cri-o/server/cri/types"
	securejoin "github.com/cyphar/filepath-securejoin"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
)

// createContainerPlatform performs platform dependent intermediate steps before calling the container's oci.Runtime().CreateContainer()
func (s *Server) createContainerPlatform(ctx context.Context, container *oci.Container, cgroupParent string, idMappings *idtools.IDMappings) error {
	if idMappings != nil && !container.Spoofed() {
		rootPair := idMappings.RootPair()
		for _, path := range []string{container.BundlePath(), container.MountPoint()} {
			if err := makeAccessible(path, rootPair.UID, rootPair.GID, false); err != nil {
				return errors.Wrapf(err, "cannot make %s accessible to %d:%d", path, rootPair.UID, rootPair.GID)
			}
		}
		if err := makeMountsAccessible(rootPair.UID, rootPair.GID, container.Spec().Mounts); err != nil {
			return err
		}
	}
	return s.Runtime().CreateContainer(ctx, container, cgroupParent)
}

// makeAccessible changes the path permission and each parent directory to have --x--x--x
func makeAccessible(path string, uid, gid int, doChown bool) error {
	if doChown {
		if err := os.Chown(path, uid, gid); err != nil {
			return errors.Wrapf(err, "cannot chown %s to %d:%d", path, uid, gid)
		}
	}
	for ; path != "/"; path = filepath.Dir(path) {
		st, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if int(st.Sys().(*syscall.Stat_t).Uid) == uid && int(st.Sys().(*syscall.Stat_t).Gid) == gid {
			continue
		}
		if st.Mode()&0o111 != 0o111 {
			if err := os.Chmod(path, st.Mode()|0o111); err != nil {
				return err
			}
		}
	}
	return nil
}

// makeMountsAccessible makes sure all the mounts are accessible from the user namespace
func makeMountsAccessible(uid, gid int, mounts []rspec.Mount) error {
	for _, m := range mounts {
		if m.Type == "bind" || util.StringInSlice("bind", m.Options) {
			if err := makeAccessible(m.Source, uid, gid, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func toContainer(id uint32, idMap []idtools.IDMap) uint32 {
	hostID := int(id)
	if idMap == nil {
		return uint32(hostID)
	}
	for _, m := range idMap {
		if hostID >= m.HostID && hostID < m.HostID+m.Size {
			contID := m.ContainerID + (hostID - m.HostID)
			return uint32(contID)
		}
	}
	// If the ID cannot be mapped, it means the RunAsUser or RunAsGroup was not specified
	// so just use the original value.
	return id
}

// finalizeUserMapping changes the UID, GID and additional GIDs to reflect the new value in the user namespace.
func (s *Server) finalizeUserMapping(specgen *generate.Generator, mappings *idtools.IDMappings) {
	if mappings == nil {
		return
	}

	// if the namespace was configured because of a static configuration, do not attempt any mapping
	if s.defaultIDMappings != nil && !s.defaultIDMappings.Empty() {
		return
	}

	specgen.Config.Process.User.UID = toContainer(specgen.Config.Process.User.UID, mappings.UIDs())
	gids := mappings.GIDs()
	specgen.Config.Process.User.GID = toContainer(specgen.Config.Process.User.GID, gids)
	for i := range specgen.Config.Process.User.AdditionalGids {
		gid := toContainer(specgen.Config.Process.User.AdditionalGids[i], gids)
		specgen.Config.Process.User.AdditionalGids[i] = gid
	}
}

func (s *Server) createSandboxContainer(ctx context.Context, ctr ctrIface.Container, sb *sandbox.Sandbox) (cntr *oci.Container, retErr error) {
	// TODO: simplify this function (cyclomatic complexity here is high)
	// TODO: factor generating/updating the spec into something other projects can vendor

	// eventually, we'd like to access all of these variables through the interface themselves, and do most
	// of the translation between CRI config -> oci/storage container in the container package
	containerID := ctr.ID()
	containerName := ctr.Name()
	containerConfig := ctr.Config()
	if err := ctr.SetPrivileged(); err != nil {
		return nil, err
	}
	securityContext := containerConfig.Linux.SecurityContext

	// creates a spec Generator with the default spec.
	specgen := ctr.Spec()
	specgen.HostSpecific = true
	specgen.ClearProcessRlimits()

	for _, u := range s.config.Ulimits() {
		specgen.AddProcessRlimits(u.Name, u.Hard, u.Soft)
	}

	readOnlyRootfs := ctr.ReadOnly(s.config.ReadOnly)
	specgen.SetRootReadonly(readOnlyRootfs)

	if s.config.ReadOnly {
		// tmpcopyup is a runc extension and is not part of the OCI spec.
		// WORK ON: Use "overlay" mounts as an alternative to tmpfs with tmpcopyup
		// Look at https://github.com/cri-o/cri-o/pull/1434#discussion_r177200245 for more info on this
		options := []string{"rw", "noexec", "nosuid", "nodev", "tmpcopyup"}
		mounts := map[string]string{
			"/run":     "mode=0755",
			"/tmp":     "mode=1777",
			"/var/tmp": "mode=1777",
		}
		for target, mode := range mounts {
			if !isInCRIMounts(target, containerConfig.Mounts) {
				ctr.SpecAddMount(rspec.Mount{
					Destination: target,
					Type:        "tmpfs",
					Source:      "tmpfs",
					Options:     append(options, mode),
				})
			}
		}
	}

	image, err := ctr.Image()
	if err != nil {
		return nil, err
	}
	images, err := s.StorageImageServer().ResolveNames(s.config.SystemContext, image)
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
		imgResult, imgResultErr = s.StorageImageServer().ImageStatus(s.config.SystemContext, img)
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

	labelOptions, err := ctr.SelinuxLabel(sb.ProcessLabel())
	if err != nil {
		return nil, err
	}

	containerIDMappings, err := s.getSandboxIDMappings(sb)
	if err != nil {
		return nil, err
	}

	var idMappingOptions *cstorage.IDMappingOptions
	if containerIDMappings != nil {
		idMappingOptions = &cstorage.IDMappingOptions{UIDMap: containerIDMappings.UIDs(), GIDMap: containerIDMappings.GIDs()}
	}

	metadata := containerConfig.Metadata

	containerInfo, err := s.StorageRuntimeServer().CreateContainer(s.config.SystemContext,
		sb.Name(), sb.ID(),
		image, imgResult.ID,
		containerName, containerID,
		metadata.Name,
		metadata.Attempt,
		idMappingOptions,
		labelOptions,
		ctr.Privileged(),
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			log.Infof(ctx, "CreateCtrLinux: deleting container %s from storage", containerInfo.ID)
			if err := s.StorageRuntimeServer().DeleteContainer(containerInfo.ID); err != nil {
				log.Warnf(ctx, "Failed to cleanup container directory: %v", err)
			}
		}
	}()

	mountLabel := containerInfo.MountLabel
	var processLabel string
	if !ctr.Privileged() {
		processLabel = containerInfo.ProcessLabel
	}
	hostIPC := securityContext.NamespaceOptions.Ipc == types.NamespaceModeNODE
	hostPID := securityContext.NamespaceOptions.Pid == types.NamespaceModeNODE
	hostNet := securityContext.NamespaceOptions.Network == types.NamespaceModeNODE

	// Don't use SELinux separation with Host Pid or IPC Namespace or privileged.
	if hostPID || hostIPC {
		processLabel, mountLabel = "", ""
	}

	if hostNet {
		processLabel = ""
	}

	containerVolumes, ociMounts, err := addOCIBindMounts(ctx, mountLabel, containerConfig, specgen, s.config.RuntimeConfig.BindMountPrefix, s.config.AbsentMountSourcesToReject)
	if err != nil {
		return nil, err
	}

	configuredDevices := s.config.Devices()

	privilegedWithoutHostDevices, err := s.Runtime().PrivilegedWithoutHostDevices(sb.RuntimeHandler())
	if err != nil {
		return nil, err
	}

	annotationDevices, err := device.DevicesFromAnnotation(sb.Annotations()[crioann.DevicesAnnotation])
	if err != nil {
		return nil, err
	}

	if err := ctr.SpecAddDevices(configuredDevices, annotationDevices, privilegedWithoutHostDevices); err != nil {
		return nil, err
	}

	labels := containerConfig.Labels

	if err := validateLabels(labels); err != nil {
		return nil, err
	}

	// set this container's apparmor profile if it is set by sandbox
	if s.Config().AppArmor().IsEnabled() && !ctr.Privileged() {
		profile, err := s.Config().AppArmor().Apply(
			securityContext.ApparmorProfile,
		)
		if err != nil {
			return nil, errors.Wrapf(err, "applying apparmor profile to container %s", containerID)
		}

		log.Debugf(ctx, "Applied AppArmor profile %s to container %s", profile, containerID)
		specgen.SetProcessApparmorProfile(profile)
	}

	logPath, err := ctr.LogPath(sb.LogDir())
	if err != nil {
		return nil, err
	}

	specgen.SetProcessTerminal(containerConfig.Tty)
	if containerConfig.Tty {
		specgen.AddProcessEnv("TERM", "xterm")
	}

	linux := containerConfig.Linux
	if linux != nil {
		resources := linux.Resources
		if resources != nil {
			specgen.SetLinuxResourcesCPUPeriod(uint64(resources.CPUPeriod))
			specgen.SetLinuxResourcesCPUQuota(resources.CPUQuota)
			specgen.SetLinuxResourcesCPUShares(uint64(resources.CPUShares))

			memoryLimit := resources.MemoryLimitInBytes
			if memoryLimit != 0 {
				if err := cgmgr.VerifyMemoryIsEnough(memoryLimit); err != nil {
					return nil, err
				}
				specgen.SetLinuxResourcesMemoryLimit(memoryLimit)
				if node.CgroupHasMemorySwap() {
					specgen.SetLinuxResourcesMemorySwap(memoryLimit)
				}
			}

			specgen.SetProcessOOMScoreAdj(int(resources.OomScoreAdj))
			specgen.SetLinuxResourcesCPUCpus(resources.CPUsetCPUs)
			specgen.SetLinuxResourcesCPUMems(resources.CPUsetMems)

			// If the kernel has no support for hugetlb, silently ignore the limits
			if node.CgroupHasHugetlb() {
				hugepageLimits := resources.HugepageLimits
				for _, limit := range hugepageLimits {
					specgen.AddLinuxResourcesHugepageLimit(limit.PageSize, limit.Limit)
				}
			}
		}

		specgen.SetLinuxCgroupsPath(s.config.CgroupManager().ContainerCgroupPath(sb.CgroupParent(), containerID))

		if ctr.Privileged() {
			specgen.SetupPrivileged(true)
		} else {
			capabilities := securityContext.Capabilities
			// Ensure we don't get a nil pointer error if the config
			// doesn't set any capabilities
			if capabilities == nil {
				capabilities = &types.Capability{}
			}
			// Clear default capabilities from spec
			specgen.ClearProcessCapabilities()
			err = setupCapabilities(specgen, capabilities, s.config.DefaultCapabilities)
			if err != nil {
				return nil, err
			}
		}
		specgen.SetProcessNoNewPrivileges(securityContext.NoNewPrivs)

		if !ctr.Privileged() {
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
				"/sys/dev",
			} {
				specgen.AddLinuxMaskedPaths(mp)
			}
			if securityContext.MaskedPaths != nil {
				specgen.Config.Linux.MaskedPaths = nil
				for _, path := range securityContext.MaskedPaths {
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
			if securityContext.ReadonlyPaths != nil {
				specgen.Config.Linux.ReadonlyPaths = nil
				for _, path := range securityContext.ReadonlyPaths {
					specgen.AddLinuxReadonlyPaths(path)
				}
			}
		}
	}

	if err := ctr.AddUnifiedResourcesFromAnnotations(sb.Annotations()); err != nil {
		return nil, err
	}

	// Join the namespace paths for the pod sandbox container.
	if err := configureGeneratorGivenNamespacePaths(sb.NamespacePaths(), specgen); err != nil {
		return nil, errors.Wrap(err, "failed to configure namespaces in container create")
	}

	if securityContext.NamespaceOptions.Pid == types.NamespaceModeNODE {
		// kubernetes PodSpec specify to use Host PID namespace
		if err := specgen.RemoveLinuxNamespace(string(rspec.PIDNamespace)); err != nil {
			return nil, err
		}
	} else if securityContext.NamespaceOptions.Pid == types.NamespaceModePOD {
		pidNsPath := sb.PidNsPath()
		if pidNsPath == "" {
			return nil, errors.New("PID namespace requested, but sandbox infra container invalid")
		}

		if err := specgen.AddOrReplaceLinuxNamespace(string(rspec.PIDNamespace), pidNsPath); err != nil {
			return nil, err
		}
	}

	// If the sandbox is configured to run in the host network, do not create a new network namespace
	if sb.HostNetwork() {
		if err := specgen.RemoveLinuxNamespace(string(rspec.NetworkNamespace)); err != nil {
			return nil, err
		}

		if !isInCRIMounts("/sys", containerConfig.Mounts) {
			specgen.RemoveMount("/sys")
			ctr.SpecAddMount(rspec.Mount{
				Destination: "/sys",
				Type:        "sysfs",
				Source:      "sysfs",
				Options:     []string{"nosuid", "noexec", "nodev", "ro"},
			})
		}
	}

	if ctr.Privileged() {
		ctr.SpecAddMount(rspec.Mount{
			Destination: "/sys",
			Type:        "sysfs",
			Source:      "sysfs",
			Options:     []string{"nosuid", "noexec", "nodev", "rw", "rslave"},
		})
		ctr.SpecAddMount(rspec.Mount{
			Destination: "/sys/fs/cgroup",
			Type:        "cgroup",
			Source:      "cgroup",
			Options:     []string{"nosuid", "noexec", "nodev", "rw", "relatime", "rslave"},
		})
	}

	containerImageConfig := containerInfo.Config
	if containerImageConfig == nil {
		err = fmt.Errorf("empty image config for %s", image)
		return nil, err
	}

	if err := ctr.SpecSetProcessArgs(containerImageConfig); err != nil {
		return nil, err
	}

	if ctr.WillRunSystemd() {
		processLabel, err = selinux.InitLabel(processLabel)
		if err != nil {
			return nil, err
		}
		setupSystemd(specgen.Mounts(), *specgen)
	}

	// When running on cgroupv2, automatically add a cgroup namespace for not privileged containers.
	if !ctr.Privileged() && node.CgroupIsV2() {
		if err := specgen.AddOrReplaceLinuxNamespace(string(rspec.CgroupNamespace), ""); err != nil {
			return nil, err
		}
	}

	ctr.SpecAddMount(rspec.Mount{
		Destination: "/dev/shm",
		Type:        "bind",
		Source:      sb.ShmPath(),
		Options:     []string{"rw", "bind"},
	})

	options := []string{"rw"}
	if readOnlyRootfs {
		options = []string{"ro"}
	}
	if sb.ResolvPath() != "" {
		if err := securityLabel(sb.ResolvPath(), mountLabel, false); err != nil {
			return nil, err
		}
		ctr.SpecAddMount(rspec.Mount{
			Destination: "/etc/resolv.conf",
			Type:        "bind",
			Source:      sb.ResolvPath(),
			Options:     append(options, []string{"bind", "nodev", "nosuid", "noexec"}...),
		})
	}

	if sb.HostnamePath() != "" {
		if err := securityLabel(sb.HostnamePath(), mountLabel, false); err != nil {
			return nil, err
		}
		ctr.SpecAddMount(rspec.Mount{
			Destination: "/etc/hostname",
			Type:        "bind",
			Source:      sb.HostnamePath(),
			Options:     append(options, "bind"),
		})
	}

	if !isInCRIMounts("/etc/hosts", containerConfig.Mounts) && hostNetwork(containerConfig) {
		// Only bind mount for host netns and when CRI does not give us any hosts file
		ctr.SpecAddMount(rspec.Mount{
			Destination: "/etc/hosts",
			Type:        "bind",
			Source:      "/etc/hosts",
			Options:     append(options, "bind"),
		})
	}

	if ctr.Privileged() {
		setOCIBindMountsPrivileged(specgen)
	}

	// Set hostname and add env for hostname
	specgen.SetHostname(sb.Hostname())
	specgen.AddProcessEnv("HOSTNAME", sb.Hostname())

	created := time.Now()
	if !ctr.Privileged() {
		if err := s.Config().Seccomp().Setup(
			ctx,
			specgen,
			securityContext.Seccomp,
			containerConfig.Linux.SecurityContext.SeccompProfilePath,
		); err != nil {
			return nil, errors.Wrap(err, "setup seccomp")
		}
	}

	mountPoint, err := s.StorageRuntimeServer().StartContainer(containerID)
	if err != nil {
		return nil, fmt.Errorf("failed to mount container %s(%s): %v", containerName, containerID, err)
	}

	defer func() {
		if retErr != nil {
			log.Infof(ctx, "CreateCtrLinux: stopping storage container %s", containerID)
			if err := s.StorageRuntimeServer().StopContainer(containerID); err != nil {
				log.Warnf(ctx, "Couldn't stop storage container: %v: %v", containerID, err)
			}
		}
	}()

	// TODO: eventually, this should be in the container package, but it's going through a lot of churn
	// and SpecAddAnnotations is already passed too many arguments
	if err := s.Runtime().FilterDisallowedAnnotations(sb.RuntimeHandler(), ctr.Config().Annotations); err != nil {
		return nil, err
	}

	// Get RDT class
	rdtClass, err := s.Config().Rdt().ContainerClassFromAnnotations(metadata.Name, containerConfig.Annotations, sb.Annotations())
	if err != nil {
		return nil, err
	}
	if rdtClass != "" {
		log.Debugf(ctx, "Setting RDT ClosID of container %s to %q", containerID, rdt.ResctrlPrefix+rdtClass)
		// TODO: patch runtime-tools to support setting ClosID via a helper func similar to SetLinuxIntelRdtL3CacheSchema()
		specgen.Config.Linux.IntelRdt = &rspec.LinuxIntelRdt{ClosID: rdt.ResctrlPrefix + rdtClass}
	}

	err = ctr.SpecAddAnnotations(ctx, sb, containerVolumes, mountPoint, containerImageConfig.Config.StopSignal, imgResult, s.config.CgroupManager().IsSystemd(), node.SystemdHasCollectMode())
	if err != nil {
		return nil, err
	}

	if err := s.config.Workloads.MutateSpecGivenAnnotations(ctr.Config().Metadata.Name, ctr.Spec(), sb.Annotations()); err != nil {
		return nil, err
	}

	// First add any configured environment variables from crio config.
	// They will get overridden if specified in the image or container config.
	specgen.AddMultipleProcessEnv(s.Config().DefaultEnv)

	// Add environment variables from image the CRI configuration
	envs := mergeEnvs(containerImageConfig, containerConfig.Envs)
	for _, e := range envs {
		parts := strings.SplitN(e, "=", 2)
		specgen.AddProcessEnv(parts[0], parts[1])
	}

	// Setup user and groups
	if linux != nil {
		if err := setupContainerUser(ctx, specgen, mountPoint, mountLabel, containerInfo.RunDir, securityContext, containerImageConfig); err != nil {
			return nil, err
		}
	}

	// Add image volumes
	volumeMounts, err := addImageVolumes(ctx, mountPoint, s, &containerInfo, mountLabel, specgen)
	if err != nil {
		return nil, err
	}

	// Set working directory
	// Pick it up from image config first and override if specified in CRI
	containerCwd := "/"
	imageCwd := containerImageConfig.Config.WorkingDir
	if imageCwd != "" {
		containerCwd = imageCwd
	}
	runtimeCwd := containerConfig.WorkingDir
	if runtimeCwd != "" {
		containerCwd = runtimeCwd
	}
	specgen.SetProcessCwd(containerCwd)
	if err := setupWorkingDirectory(mountPoint, mountLabel, containerCwd); err != nil {
		return nil, err
	}

	// Add secrets from the default and override mounts.conf files
	secretMounts := secrets.SecretMounts(mountLabel, containerInfo.RunDir, s.config.DefaultMountsFile, rootless.IsRootless(), ctr.DisableFips())

	mounts := []rspec.Mount{}
	mounts = append(mounts, ociMounts...)
	mounts = append(mounts, volumeMounts...)
	mounts = append(mounts, secretMounts...)

	sort.Sort(orderedMounts(mounts))

	for _, m := range mounts {
		rspecMount := rspec.Mount{
			Type:        "bind",
			Options:     append(m.Options, "bind"),
			Destination: m.Destination,
			Source:      m.Source,
		}
		ctr.SpecAddMount(rspecMount)
	}

	if s.ContainerServer.Hooks != nil {
		newAnnotations := map[string]string{}
		for key, value := range containerConfig.Annotations {
			newAnnotations[key] = value
		}
		for key, value := range sb.Annotations() {
			newAnnotations[key] = value
		}

		if _, err := s.ContainerServer.Hooks.Hooks(specgen.Config, newAnnotations, len(containerConfig.Mounts) > 0); err != nil {
			return nil, err
		}
	}

	// Set up pids limit if pids cgroup is mounted
	if node.CgroupHasPid() {
		specgen.SetLinuxResourcesPidsLimit(s.config.PidsLimit)
	}

	// by default, the root path is an empty string. set it now.
	specgen.SetRootPath(mountPoint)

	crioAnnotations := specgen.Config.Annotations

	ociMetadata := &oci.Metadata{
		Name:    metadata.Name,
		Attempt: metadata.Attempt,
	}
	ociContainer, err := oci.NewContainer(containerID, containerName, containerInfo.RunDir, logPath, labels, crioAnnotations, ctr.Config().Annotations, image, imageName, imageRef, ociMetadata, sb.ID(), containerConfig.Tty, containerConfig.Stdin, containerConfig.StdinOnce, sb.RuntimeHandler(), containerInfo.Dir, created, containerImageConfig.Config.StopSignal)
	if err != nil {
		return nil, err
	}

	specgen.SetLinuxMountLabel(mountLabel)
	specgen.SetProcessSelinuxLabel(processLabel)

	ociContainer.SetIDMappings(containerIDMappings)
	if containerIDMappings != nil {
		s.finalizeUserMapping(specgen, containerIDMappings)

		for _, uidmap := range containerIDMappings.UIDs() {
			specgen.AddLinuxUIDMapping(uint32(uidmap.HostID), uint32(uidmap.ContainerID), uint32(uidmap.Size))
		}
		for _, gidmap := range containerIDMappings.GIDs() {
			specgen.AddLinuxGIDMapping(uint32(gidmap.HostID), uint32(gidmap.ContainerID), uint32(gidmap.Size))
		}

		rootPair := containerIDMappings.RootPair()

		pathsToChown := []string{mountPoint, containerInfo.RunDir}
		for _, m := range secretMounts {
			pathsToChown = append(pathsToChown, m.Source)
		}
		for _, path := range pathsToChown {
			if err := makeAccessible(path, rootPair.UID, rootPair.GID, true); err != nil {
				return nil, errors.Wrapf(err, "cannot chown %s to %d:%d", path, rootPair.UID, rootPair.GID)
			}
		}
	} else if err := specgen.RemoveLinuxNamespace(string(rspec.UserNamespace)); err != nil {
		return nil, err
	}

	if os.Getenv(rootlessEnvName) != "" {
		makeOCIConfigurationRootless(specgen)
	}

	saveOptions := generate.ExportOptions{}
	if err := specgen.SaveToFile(filepath.Join(containerInfo.Dir, "config.json"), saveOptions); err != nil {
		return nil, err
	}

	if err := specgen.SaveToFile(filepath.Join(containerInfo.RunDir, "config.json"), saveOptions); err != nil {
		return nil, err
	}

	ociContainer.SetSpec(specgen.Config)
	ociContainer.SetMountPoint(mountPoint)
	ociContainer.SetSeccompProfilePath(containerConfig.Linux.SecurityContext.SeccompProfilePath)

	for _, cv := range containerVolumes {
		ociContainer.AddVolume(cv)
	}

	return ociContainer, nil
}

func setupWorkingDirectory(rootfs, mountLabel, containerCwd string) error {
	fp, err := securejoin.SecureJoin(rootfs, containerCwd)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(fp, 0o755); err != nil {
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
	spec := g.Config
	// clear readonly for /sys and cgroup
	for i := range spec.Mounts {
		clearReadOnly(&spec.Mounts[i])
	}
	spec.Linux.ReadonlyPaths = nil
	spec.Linux.MaskedPaths = nil
}

func clearReadOnly(m *rspec.Mount) {
	var opt []string
	for _, o := range m.Options {
		if o == "rw" {
			return
		} else if o != "ro" {
			opt = append(opt, o)
		}
	}
	m.Options = opt
	m.Options = append(m.Options, "rw")
}

func addOCIBindMounts(ctx context.Context, mountLabel string, containerConfig *types.ContainerConfig, specgen *generate.Generator, bindMountPrefix string, absentMountSourcesToReject []string) ([]oci.ContainerVolume, []rspec.Mount, error) {
	volumes := []oci.ContainerVolume{}
	ociMounts := []rspec.Mount{}
	mounts := containerConfig.Mounts

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
		if _, mountSys := mountSet["/sys"]; mountSys && strings.HasPrefix(dst, "/sys/") {
			// filter out everything under /sys if /sys is a supplied mount
			continue
		}
		specgen.AddMount(m)
	}

	mountInfos, err := mount.GetMounts()
	if err != nil {
		return nil, nil, err
	}
	for _, m := range mounts {
		dest := m.ContainerPath
		if dest == "" {
			return nil, nil, fmt.Errorf("mount.ContainerPath is empty")
		}
		if m.HostPath == "" {
			return nil, nil, fmt.Errorf("mount.HostPath is empty")
		}
		if m.HostPath == "/" && dest == "/" {
			log.Warnf(ctx, "Configuration specifies mounting host root to the container root.  This is dangerous (especially with privileged containers) and should be avoided.")
		}
		src := filepath.Join(bindMountPrefix, m.HostPath)

		resolvedSrc, err := resolveSymbolicLink(bindMountPrefix, src)
		if err == nil {
			src = resolvedSrc
		} else {
			if !os.IsNotExist(err) {
				return nil, nil, fmt.Errorf("failed to resolve symlink %q: %v", src, err)
			}
			for _, toReject := range absentMountSourcesToReject {
				if filepath.Clean(src) == toReject {
					// special-case /etc/hostname, as we don't want it to be created as a directory
					// This can cause issues with node reboot.
					return nil, nil, errors.Errorf("Cannot mount %s: path does not exist and will cause issues as a directory", toReject)
				}
			}
			if err = os.MkdirAll(src, 0o755); err != nil {
				return nil, nil, fmt.Errorf("failed to mkdir %s: %s", src, err)
			}
		}

		options := []string{"rw"}
		if m.Readonly {
			options = []string{"ro"}
		}
		options = append(options, "rbind")

		// mount propagation
		switch m.Propagation {
		case types.MountPropagationPropagationPrivate:
			options = append(options, "rprivate")
			// Since default root propagation in runc is rprivate ignore
			// setting the root propagation
		case types.MountPropagationPropagationBidirectional:
			if err := ensureShared(src, mountInfos); err != nil {
				return nil, nil, err
			}
			options = append(options, "rshared")
			if err := specgen.SetLinuxRootPropagation("rshared"); err != nil {
				return nil, nil, err
			}
		case types.MountPropagationPropagationHostToContainer:
			if err := ensureSharedOrSlave(src, mountInfos); err != nil {
				return nil, nil, err
			}
			options = append(options, "rslave")
			if specgen.Config.Linux.RootfsPropagation != "rshared" &&
				specgen.Config.Linux.RootfsPropagation != "rslave" {
				if err := specgen.SetLinuxRootPropagation("rslave"); err != nil {
					return nil, nil, err
				}
			}
		default:
			log.Warnf(ctx, "Unknown propagation mode for hostPath %q", m.HostPath)
			options = append(options, "rprivate")
		}

		if m.SelinuxRelabel {
			if err := securityLabel(src, mountLabel, false); err != nil {
				return nil, nil, err
			}
		}

		volumes = append(volumes, oci.ContainerVolume{
			ContainerPath: dest,
			HostPath:      src,
			Readonly:      m.Readonly,
		})

		ociMounts = append(ociMounts, rspec.Mount{
			Source:      src,
			Destination: dest,
			Options:     options,
		})
	}

	if _, mountSys := mountSet["/sys"]; !mountSys {
		m := rspec.Mount{
			Destination: "/sys/fs/cgroup",
			Type:        "cgroup",
			Source:      "cgroup",
			Options:     []string{"nosuid", "noexec", "nodev", "relatime", "ro"},
		}
		specgen.AddMount(m)
	}

	return volumes, ociMounts, nil
}

// mountExists returns true if dest exists in the list of mounts
func mountExists(specMounts []rspec.Mount, dest string) bool {
	for _, m := range specMounts {
		if m.Destination == dest {
			return true
		}
	}
	return false
}

// systemd expects to have /run, /run/lock and /tmp on tmpfs
// It also expects to be able to write to /sys/fs/cgroup/systemd and /var/log/journal
func setupSystemd(mounts []rspec.Mount, g generate.Generator) {
	options := []string{"rw", "rprivate", "noexec", "nosuid", "nodev"}
	for _, dest := range []string{"/run", "/run/lock"} {
		if mountExists(mounts, dest) {
			continue
		}
		tmpfsMnt := rspec.Mount{
			Destination: dest,
			Type:        "tmpfs",
			Source:      "tmpfs",
			Options:     append(options, "tmpcopyup"),
		}
		g.AddMount(tmpfsMnt)
	}
	for _, dest := range []string{"/tmp", "/var/log/journal"} {
		if mountExists(mounts, dest) {
			continue
		}
		tmpfsMnt := rspec.Mount{
			Destination: dest,
			Type:        "tmpfs",
			Source:      "tmpfs",
			Options:     append(options, "tmpcopyup"),
		}
		g.AddMount(tmpfsMnt)
	}

	if node.CgroupIsV2() {
		g.RemoveMount("/sys/fs/cgroup")

		systemdMnt := rspec.Mount{
			Destination: "/sys/fs/cgroup",
			Type:        "cgroup",
			Source:      "cgroup",
			Options:     []string{"private", "rw"},
		}
		g.AddMount(systemdMnt)
	} else {
		systemdMnt := rspec.Mount{
			Destination: "/sys/fs/cgroup/systemd",
			Type:        "bind",
			Source:      "/sys/fs/cgroup/systemd",
			Options:     []string{"bind", "nodev", "noexec", "nosuid"},
		}
		g.AddMount(systemdMnt)
		g.AddLinuxMaskedPaths("/sys/fs/cgroup/systemd/release_agent")
	}
	g.AddProcessEnv("container", "crio")
}
