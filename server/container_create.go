package server

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/containers/storage/pkg/idtools"
	"github.com/containers/storage/pkg/mount"
	"github.com/containers/storage/pkg/stringid"
	"github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/log"
	"github.com/cri-o/cri-o/internal/resourcestore"
	"github.com/cri-o/cri-o/internal/storage"
	"github.com/cri-o/cri-o/pkg/config"
	"github.com/cri-o/cri-o/pkg/container"
	"github.com/cri-o/cri-o/utils"
	securejoin "github.com/cyphar/filepath-securejoin"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/pkg/errors"
	seccomp "github.com/seccomp/containers-golang"
	k8sV1 "k8s.io/api/core/v1"
	pb "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

// sync with https://github.com/containers/storage/blob/7fe03f6c765f2adbc75a5691a1fb4f19e56e7071/pkg/truncindex/truncindex.go#L92
const noSuchID = "no such id"

type orderedMounts []rspec.Mount

// Len returns the number of mounts. Used in sorting.
func (m orderedMounts) Len() int {
	return len(m)
}

// Less returns true if the number of parts (a/b/c would be 3 parts) in the
// mount indexed by parameter 1 is less than that of the mount indexed by
// parameter 2. Used in sorting.
func (m orderedMounts) Less(i, j int) bool {
	return m.parts(i) < m.parts(j)
}

// Swap swaps two items in an array of mounts. Used in sorting
func (m orderedMounts) Swap(i, j int) {
	m[i], m[j] = m[j], m[i]
}

// parts returns the number of parts in the destination of a mount. Used in sorting.
func (m orderedMounts) parts(i int) int {
	return strings.Count(filepath.Clean(m[i].Destination), string(os.PathSeparator))
}

// mounts defines how to sort runtime.Mount.
// This is the same with the Docker implementation:
//   https://github.com/moby/moby/blob/17.05.x/daemon/volumes.go#L26
type criOrderedMounts []*pb.Mount

// Len returns the number of mounts. Used in sorting.
func (m criOrderedMounts) Len() int {
	return len(m)
}

// Less returns true if the number of parts (a/b/c would be 3 parts) in the
// mount indexed by parameter 1 is less than that of the mount indexed by
// parameter 2. Used in sorting.
func (m criOrderedMounts) Less(i, j int) bool {
	return m.parts(i) < m.parts(j)
}

// Swap swaps two items in an array of mounts. Used in sorting
func (m criOrderedMounts) Swap(i, j int) {
	m[i], m[j] = m[j], m[i]
}

// parts returns the number of parts in the destination of a mount. Used in sorting.
func (m criOrderedMounts) parts(i int) int {
	return strings.Count(filepath.Clean(m[i].ContainerPath), string(os.PathSeparator))
}

// Ensure mount point on which path is mounted, is shared.
func ensureShared(path string, mountInfos []*mount.Info) error {
	sourceMount, optionalOpts, err := getSourceMount(path, mountInfos)
	if err != nil {
		return err
	}

	// Make sure source mount point is shared.
	optsSplit := strings.Split(optionalOpts, " ")
	for _, opt := range optsSplit {
		if strings.HasPrefix(opt, "shared:") {
			return nil
		}
	}

	return fmt.Errorf("path %q is mounted on %q but it is not a shared mount", path, sourceMount)
}

// Ensure mount point on which path is mounted, is either shared or slave.
func ensureSharedOrSlave(path string, mountInfos []*mount.Info) error {
	sourceMount, optionalOpts, err := getSourceMount(path, mountInfos)
	if err != nil {
		return err
	}
	// Make sure source mount point is shared.
	optsSplit := strings.Split(optionalOpts, " ")
	for _, opt := range optsSplit {
		if strings.HasPrefix(opt, "shared:") {
			return nil
		} else if strings.HasPrefix(opt, "master:") {
			return nil
		}
	}
	return fmt.Errorf("path %q is mounted on %q but it is not a shared or slave mount", path, sourceMount)
}

func addImageVolumes(ctx context.Context, rootfs string, s *Server, containerInfo *storage.ContainerInfo, mountLabel string, specgen *generate.Generator) ([]rspec.Mount, error) {
	mounts := []rspec.Mount{}
	for dest := range containerInfo.Config.Config.Volumes {
		fp, err := securejoin.SecureJoin(rootfs, dest)
		if err != nil {
			return nil, err
		}
		switch s.config.ImageVolumes {
		case config.ImageVolumesMkdir:
			IDs := idtools.IDPair{UID: int(specgen.Config.Process.User.UID), GID: int(specgen.Config.Process.User.GID)}
			if err1 := idtools.MkdirAllAndChownNew(fp, 0o755, IDs); err1 != nil {
				return nil, err1
			}
			if mountLabel != "" {
				if err1 := securityLabel(fp, mountLabel, true); err1 != nil {
					return nil, err1
				}
			}
		case config.ImageVolumesBind:
			volumeDirName := stringid.GenerateNonCryptoID()
			src := filepath.Join(containerInfo.RunDir, "mounts", volumeDirName)
			if err1 := os.MkdirAll(src, 0o755); err1 != nil {
				return nil, err1
			}
			// Label the source with the sandbox selinux mount label
			if mountLabel != "" {
				if err1 := securityLabel(src, mountLabel, true); err1 != nil {
					return nil, err1
				}
			}

			log.Debugf(ctx, "Adding bind mounted volume: %s to %s", src, dest)
			mounts = append(mounts, rspec.Mount{
				Source:      src,
				Destination: dest,
				Type:        "bind",
				Options:     []string{"private", "bind", "rw"},
			})

		case config.ImageVolumesIgnore:
			log.Debugf(ctx, "ignoring volume %v", dest)
		default:
			log.Errorf(ctx, "unrecognized image volumes setting")
		}
	}
	return mounts, nil
}

// resolveSymbolicLink resolves a possbile symlink path. If the path is a symlink, returns resolved
// path; if not, returns the original path.
func resolveSymbolicLink(path, scope string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != os.ModeSymlink {
		return path, nil
	}
	if scope == "" {
		scope = "/"
	}
	return securejoin.SecureJoin(scope, path)
}

func addDevices(ctx context.Context, sb *sandbox.Sandbox, containerConfig *pb.ContainerConfig, privilegedWithoutHostDevices bool, specgen *generate.Generator) error {
	return addDevicesPlatform(ctx, sb, containerConfig, privilegedWithoutHostDevices, specgen)
}

// buildOCIProcessArgs build an OCI compatible process arguments slice.
func buildOCIProcessArgs(ctx context.Context, containerKubeConfig *pb.ContainerConfig, imageOCIConfig *v1.Image) ([]string, error) {
	// # Start the nginx container using the default command, but use custom
	// arguments (arg1 .. argN) for that command.
	// kubectl run nginx --image=nginx -- <arg1> <arg2> ... <argN>

	// # Start the nginx container using a different command and custom arguments.
	// kubectl run nginx --image=nginx --command -- <cmd> <arg1> ... <argN>

	kubeCommands := containerKubeConfig.Command
	kubeArgs := containerKubeConfig.Args

	// merge image config and kube config
	// same as docker does today...
	if imageOCIConfig != nil {
		if len(kubeCommands) == 0 {
			if len(kubeArgs) == 0 {
				kubeArgs = imageOCIConfig.Config.Cmd
			}
			if kubeCommands == nil {
				kubeCommands = imageOCIConfig.Config.Entrypoint
			}
		}
	}

	if len(kubeCommands) == 0 && len(kubeArgs) == 0 {
		return nil, fmt.Errorf("no command specified")
	}

	// create entrypoint and args
	var entrypoint string
	var args []string
	if len(kubeCommands) != 0 {
		entrypoint = kubeCommands[0]
		args = kubeCommands[1:]
		args = append(args, kubeArgs...)
	} else {
		entrypoint = kubeArgs[0]
		args = kubeArgs[1:]
	}

	processArgs := append([]string{entrypoint}, args...)

	log.Debugf(ctx, "OCI process args %v", processArgs)

	return processArgs, nil
}

// setupContainerUser sets the UID, GID and supplemental groups in OCI runtime config
func setupContainerUser(ctx context.Context, specgen *generate.Generator, rootfs, mountLabel, ctrRunDir string, sc *pb.LinuxContainerSecurityContext, imageConfig *v1.Image) error {
	if sc == nil {
		return nil
	}
	if sc.GetRunAsGroup() != nil && sc.GetRunAsUser() == nil && sc.GetRunAsUsername() == "" {
		return fmt.Errorf("user group is specified without user or username")
	}
	imageUser := ""
	homedir := ""
	for _, env := range specgen.Config.Process.Env {
		if strings.HasPrefix(env, "HOME=") {
			homedir = strings.TrimPrefix(env, "HOME=")
			break
		}
	}
	if homedir == "" {
		homedir = specgen.Config.Process.Cwd
	}

	if imageConfig != nil {
		imageUser = imageConfig.Config.User
	}
	containerUser := generateUserString(
		sc.GetRunAsUsername(),
		imageUser,
		sc.GetRunAsUser(),
	)
	log.Debugf(ctx, "CONTAINER USER: %+v", containerUser)

	// Add uid, gid and groups from user
	uid, gid, addGroups, err := utils.GetUserInfo(rootfs, containerUser)
	if err != nil {
		return err
	}

	genPasswd := true
	for _, mount := range specgen.Config.Mounts {
		if mount.Destination == "/etc" ||
			mount.Destination == "/etc/" ||
			mount.Destination == "/etc/passwd" {
			genPasswd = false
			break
		}
	}
	if genPasswd {
		// verify uid exists in containers /etc/passwd, else generate a passwd with the user entry
		passwdPath, err := utils.GeneratePasswd(containerUser, uid, gid, homedir, rootfs, ctrRunDir)
		if err != nil {
			return err
		}
		if passwdPath != "" {
			if err := securityLabel(passwdPath, mountLabel, false); err != nil {
				return err
			}

			mnt := rspec.Mount{
				Type:        "bind",
				Source:      passwdPath,
				Destination: "/etc/passwd",
				Options:     []string{"rw", "bind", "nodev", "nosuid", "noexec"},
			}
			specgen.AddMount(mnt)
		}
	}

	specgen.SetProcessUID(uid)
	specgen.SetProcessGID(gid)
	if sc.GetRunAsGroup() != nil {
		specgen.SetProcessGID(uint32(sc.GetRunAsGroup().GetValue()))
	}

	for _, group := range addGroups {
		specgen.AddProcessAdditionalGid(group)
	}

	// Add groups from CRI
	groups := sc.GetSupplementalGroups()
	for _, group := range groups {
		specgen.AddProcessAdditionalGid(uint32(group))
	}
	return nil
}

// generateUserString generates valid user string based on OCI Image Spec v1.0.0.
func generateUserString(username, imageUser string, uid *pb.Int64Value) string {
	var userstr string
	if uid != nil {
		userstr = strconv.FormatInt(uid.GetValue(), 10)
	}
	if username != "" {
		userstr = username
	}
	// We use the user from the image config if nothing is provided
	if userstr == "" {
		userstr = imageUser
	}
	if userstr == "" {
		return ""
	}
	return userstr
}

// setupCapabilities sets process.capabilities in the OCI runtime config.
func setupCapabilities(specgen *generate.Generator, capabilities *pb.Capability) error {
	// Remove all ambient capabilities. Kubernetes is not yet ambient capabilities aware
	// and pods expect that switching to a non-root user results in the capabilities being
	// dropped. This should be revisited in the future.
	specgen.Config.Process.Capabilities.Ambient = []string{}

	if capabilities == nil {
		return nil
	}

	toCAPPrefixed := func(cap string) string {
		if !strings.HasPrefix(strings.ToLower(cap), "cap_") {
			return "CAP_" + strings.ToUpper(cap)
		}
		return cap
	}

	// Add/drop all capabilities if "all" is specified, so that
	// following individual add/drop could still work. E.g.
	// AddCapabilities: []string{"ALL"}, DropCapabilities: []string{"CHOWN"}
	// will be all capabilities without `CAP_CHOWN`.
	// see https://github.com/kubernetes/kubernetes/issues/51980
	if inStringSlice(capabilities.GetAddCapabilities(), "ALL") {
		for _, c := range getOCICapabilitiesList() {
			if err := specgen.AddProcessCapabilityBounding(c); err != nil {
				return err
			}
			if err := specgen.AddProcessCapabilityEffective(c); err != nil {
				return err
			}
			if err := specgen.AddProcessCapabilityInheritable(c); err != nil {
				return err
			}
			if err := specgen.AddProcessCapabilityPermitted(c); err != nil {
				return err
			}
		}
	}
	if inStringSlice(capabilities.GetDropCapabilities(), "ALL") {
		for _, c := range getOCICapabilitiesList() {
			if err := specgen.DropProcessCapabilityBounding(c); err != nil {
				return err
			}
			if err := specgen.DropProcessCapabilityEffective(c); err != nil {
				return err
			}
			if err := specgen.DropProcessCapabilityInheritable(c); err != nil {
				return err
			}
			if err := specgen.DropProcessCapabilityPermitted(c); err != nil {
				return err
			}
		}
	}

	for _, cap := range capabilities.GetAddCapabilities() {
		if strings.EqualFold(cap, "ALL") {
			continue
		}
		capPrefixed := toCAPPrefixed(cap)
		// Validate capability
		if !inStringSlice(getOCICapabilitiesList(), capPrefixed) {
			return fmt.Errorf("unknown capability %q to add", capPrefixed)
		}
		if err := specgen.AddProcessCapabilityBounding(capPrefixed); err != nil {
			return err
		}
		if err := specgen.AddProcessCapabilityEffective(capPrefixed); err != nil {
			return err
		}
		if err := specgen.AddProcessCapabilityInheritable(capPrefixed); err != nil {
			return err
		}
		if err := specgen.AddProcessCapabilityPermitted(capPrefixed); err != nil {
			return err
		}
	}

	for _, cap := range capabilities.GetDropCapabilities() {
		if strings.EqualFold(cap, "ALL") {
			continue
		}
		capPrefixed := toCAPPrefixed(cap)
		if err := specgen.DropProcessCapabilityBounding(capPrefixed); err != nil {
			return fmt.Errorf("failed to drop cap %s %v", capPrefixed, err)
		}
		if err := specgen.DropProcessCapabilityEffective(capPrefixed); err != nil {
			return fmt.Errorf("failed to drop cap %s %v", capPrefixed, err)
		}
		if err := specgen.DropProcessCapabilityInheritable(capPrefixed); err != nil {
			return fmt.Errorf("failed to drop cap %s %v", capPrefixed, err)
		}
		if err := specgen.DropProcessCapabilityPermitted(capPrefixed); err != nil {
			return fmt.Errorf("failed to drop cap %s %v", capPrefixed, err)
		}
	}

	return nil
}

func hostNetwork(containerConfig *pb.ContainerConfig) bool {
	securityContext := containerConfig.GetLinux().GetSecurityContext()
	if securityContext == nil || securityContext.GetNamespaceOptions() == nil {
		return false
	}

	return securityContext.GetNamespaceOptions().GetNetwork() == pb.NamespaceMode_NODE
}

// addSecretsBindMounts mounts user defined secrets to the container
func addSecretsBindMounts(ctx context.Context, mountLabel, ctrRunDir string, defaultMounts []string, specgen generate.Generator) ([]rspec.Mount, error) {
	containerMounts := specgen.Config.Mounts
	mounts, err := secretMounts(ctx, defaultMounts, mountLabel, ctrRunDir, containerMounts)
	if err != nil {
		return nil, err
	}
	return mounts, nil
}

// CreateContainer creates a new container in specified PodSandbox
func (s *Server) CreateContainer(ctx context.Context, req *pb.CreateContainerRequest) (res *pb.CreateContainerResponse, retErr error) {
	log.Infof(ctx, "Creating container: %s", translateLabelsToDescription(req.GetConfig().GetLabels()))

	s.updateLock.RLock()
	defer s.updateLock.RUnlock()

	sbID := req.PodSandboxId
	if sbID == "" {
		return nil, fmt.Errorf("PodSandboxId should not be empty")
	}

	sandboxID, err := s.PodIDIndex().Get(sbID)
	if err != nil {
		return nil, fmt.Errorf("PodSandbox with ID starting with %s not found: %v", sbID, err)
	}

	sb := s.getSandbox(sandboxID)
	if sb == nil {
		return nil, fmt.Errorf("specified sandbox not found: %s", sandboxID)
	}
	stopMutex := sb.StopMutex()
	stopMutex.RLock()
	defer stopMutex.RUnlock()
	if sb.Stopped() {
		return nil, fmt.Errorf("CreateContainer failed as the sandbox was stopped: %v", sbID)
	}

	ctr := container.New(ctx)
	if err := ctr.SetConfig(req.GetConfig(), req.GetSandboxConfig()); err != nil {
		return nil, errors.Wrap(err, "setting container config")
	}

	if err := ctr.SetNameAndID(); err != nil {
		return nil, errors.Wrap(err, "setting container name and ID")
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

	if _, err = s.ReserveContainerName(ctr.ID(), ctr.Name()); err != nil {
		cachedID, resourceErr := s.getResourceOrWait(ctx, ctr.Name(), "container")
		if resourceErr == nil {
			return &pb.CreateContainerResponse{ContainerId: cachedID}, nil
		}
		return nil, errors.Wrapf(err, resourceErr.Error())
	}

	description := fmt.Sprintf("createCtr: releasing container name %s", ctr.Name())
	resourceCleaner.Add(ctx, description, func() error {
		log.Infof(ctx, description)
		s.ReleaseContainerName(ctr.Name())
		return nil
	})

	newContainer, err := s.createSandboxContainer(ctx, ctr, sb)
	if err != nil {
		return nil, err
	}
	description = fmt.Sprintf("createCtr: deleting container %s from storage", ctr.ID())
	resourceCleaner.Add(ctx, description, func() error {
		log.Infof(ctx, description)
		err2 := s.StorageRuntimeServer().DeleteContainer(ctr.ID())
		if err2 != nil {
			log.Warnf(ctx, "Failed to cleanup container directory: %v", err2)
		}
		return err2
	})

	s.addContainer(newContainer)
	description = fmt.Sprintf("createCtr: removing container %s", newContainer.ID())
	resourceCleaner.Add(ctx, description, func() error {
		log.Infof(ctx, description)
		s.removeContainer(newContainer)
		return nil
	})

	if err := s.CtrIDIndex().Add(ctr.ID()); err != nil {
		return nil, err
	}
	description = fmt.Sprintf("createCtr: deleting container ID %s from idIndex", ctr.ID())
	resourceCleaner.Add(ctx, description, func() error {
		log.Infof(ctx, description)
		err := s.CtrIDIndex().Delete(ctr.ID())
		if err != nil {
			// already deleted
			if strings.Contains(err.Error(), noSuchID) {
				return nil
			}
			log.Warnf(ctx, "Couldn't delete ctr id %s from idIndex", ctr.ID())
		}
		return err
	})

	if err := s.createContainerPlatform(newContainer, sb.CgroupParent()); err != nil {
		return nil, err
	}
	description = fmt.Sprintf("createCtr: removing container ID %s from runtime", ctr.ID())
	resourceCleaner.Add(ctx, description, func() error {
		if retErr != nil {
			log.Infof(ctx, description)
			if err := s.Runtime().DeleteContainer(newContainer); err != nil {
				log.Warnf(ctx, "failed to delete container in runtime %s: %v", ctr.ID(), err)
				return err
			}
		}
		return nil
	})

	if err := s.ContainerStateToDisk(newContainer); err != nil {
		log.Warnf(ctx, "unable to write containers %s state to disk: %v", newContainer.ID(), err)
	}

	if isContextError(ctx.Err()) {
		if err := s.resourceStore.Put(ctr.Name(), newContainer, resourceCleaner); err != nil {
			log.Errorf(ctx, "createCtr: failed to save progress of container %s: %v", newContainer.ID(), err)
		}
		log.Infof(ctx, "createCtr: context was either canceled or the deadline was exceeded: %v", ctx.Err())
		return nil, ctx.Err()
	}

	newContainer.SetCreated()

	log.Infof(ctx, "Created container %s: %s", newContainer.ID(), newContainer.Description())
	return &pb.CreateContainerResponse{
		ContainerId: ctr.ID(),
	}, nil
}

func isInCRIMounts(dst string, mounts []*pb.Mount) bool {
	for _, m := range mounts {
		if m.ContainerPath == dst {
			return true
		}
	}
	return false
}

func (s *Server) setupSeccomp(ctx context.Context, specgen *generate.Generator, profile string) error {
	if profile == "" {
		// running w/o seccomp, aka unconfined
		specgen.Config.Linux.Seccomp = nil
		return nil
	}
	if s.Config().Seccomp().IsDisabled() {
		if profile != k8sV1.SeccompProfileNameUnconfined {
			return fmt.Errorf("seccomp is not enabled in your kernel, cannot run with a profile")
		}
		log.Warnf(ctx, "seccomp is not enabled in your kernel, running container without profile")
	}
	if profile == k8sV1.SeccompProfileNameUnconfined {
		// running w/o seccomp, aka unconfined
		specgen.Config.Linux.Seccomp = nil
		return nil
	}

	// Load the default seccomp profile from the server if the profile is a default one
	if profile == k8sV1.SeccompProfileRuntimeDefault || profile == k8sV1.DeprecatedSeccompProfileDockerDefault {
		linuxSpecs, err := seccomp.LoadProfileFromConfig(s.Config().Seccomp().Profile(), specgen.Config)
		if err != nil {
			return err
		}
		specgen.Config.Linux.Seccomp = linuxSpecs
		return nil
	}

	// Load local seccomp profiles including their availability validation
	if !strings.HasPrefix(profile, k8sV1.SeccompLocalhostProfileNamePrefix) {
		return fmt.Errorf("unknown seccomp profile option: %q", profile)
	}
	fname := strings.TrimPrefix(profile, k8sV1.SeccompLocalhostProfileNamePrefix)
	file, err := ioutil.ReadFile(filepath.FromSlash(fname))
	if err != nil {
		return fmt.Errorf("cannot load seccomp profile %q: %v", fname, err)
	}
	linuxSpecs, err := seccomp.LoadProfileFromBytes(file, specgen.Config)
	if err != nil {
		return err
	}
	specgen.Config.Linux.Seccomp = linuxSpecs
	return nil
}
