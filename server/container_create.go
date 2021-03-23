package server

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/log"
	"github.com/cri-o/cri-o/pkg/container"
	"github.com/cri-o/cri-o/server/cri/types"
	"github.com/cri-o/cri-o/utils"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/pkg/errors"
)

// setupContainerUser sets the UID, GID and supplemental groups in OCI runtime config
func setupContainerUser(ctx context.Context, specgen *generate.Generator, rootfs, mountLabel, ctrRunDir string, sc *types.LinuxContainerSecurityContext, imageConfig *v1.Image) error {
	if sc == nil {
		return nil
	}
	if sc.RunAsGroup != nil && sc.RunAsUser == nil && sc.RunAsUsername == "" {
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
		sc.RunAsUsername,
		imageUser,
		sc.RunAsUser,
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
			if err := container.SecurityLabel(passwdPath, mountLabel, false); err != nil {
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
	if sc.RunAsGroup != nil {
		specgen.SetProcessGID(uint32(sc.RunAsGroup.Value))
	}

	for _, group := range addGroups {
		specgen.AddProcessAdditionalGid(group)
	}

	// Add groups from CRI
	groups := sc.SupplementalGroups
	for _, group := range groups {
		specgen.AddProcessAdditionalGid(uint32(group))
	}
	return nil
}

// generateUserString generates valid user string based on OCI Image Spec v1.0.0.
func generateUserString(username, imageUser string, uid *types.Int64Value) string {
	var userstr string
	if uid != nil {
		userstr = strconv.FormatInt(uid.Value, 10)
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
func setupCapabilities(specgen *generate.Generator, capabilities *types.Capability) error {
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
	if inStringSlice(capabilities.AddCapabilities, "ALL") {
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
	if inStringSlice(capabilities.DropCapabilities, "ALL") {
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

	for _, cap := range capabilities.AddCapabilities {
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

	for _, cap := range capabilities.DropCapabilities {
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

// CreateContainer creates a new container in specified PodSandbox
func (s *Server) CreateContainer(ctx context.Context, req *types.CreateContainerRequest) (res *types.CreateContainerResponse, retErr error) {
	log.Infof(ctx, "Creating container: %s", translateLabelsToDescription(req.Config.Labels))

	s.updateLock.RLock()
	defer s.updateLock.RUnlock()
	sb, err := s.getPodSandboxFromRequest(req.PodSandboxID)
	if err != nil {
		if err == sandbox.ErrIDEmpty {
			return nil, err
		}
		return nil, errors.Wrapf(err, "specified sandbox not found: %s", req.PodSandboxID)
	}

	stopMutex := sb.StopMutex()
	stopMutex.RLock()
	defer stopMutex.RUnlock()
	if sb.Stopped() {
		return nil, fmt.Errorf("CreateContainer failed as the sandbox was stopped: %s", sb.ID())
	}

	ctr, err := container.New()
	if err != nil {
		return nil, errors.Wrap(err, "failed to create container")
	}

	if err := ctr.SetConfig(req.Config, req.SandboxConfig); err != nil {
		return nil, errors.Wrap(err, "setting container config")
	}

	if err := ctr.SetNameAndID(); err != nil {
		return nil, errors.Wrap(err, "setting container name and ID")
	}

	cleanupFuncs := make([]func(), 0)
	defer func() {
		// no error, no need to cleanup
		if retErr == nil || isContextError(retErr) {
			return
		}
		for i := len(cleanupFuncs) - 1; i >= 0; i-- {
			cleanupFuncs[i]()
		}
	}()

	if _, err = s.ReserveContainerName(ctr.ID(), ctr.Name()); err != nil {
		cachedID, resourceErr := s.getResourceOrWait(ctx, ctr.Name(), "container")
		if resourceErr == nil {
			return &types.CreateContainerResponse{ContainerID: cachedID}, nil
		}
		return nil, errors.Wrapf(err, resourceErr.Error())
	}

	cleanupFuncs = append(cleanupFuncs, func() {
		log.Infof(ctx, "createCtr: releasing container name %s", ctr.Name())
		s.ReleaseContainerName(ctr.Name())
	})

	newContainer, err := s.createSandboxContainer(ctx, ctr, sb)
	if err != nil {
		return nil, err
	}
	cleanupFuncs = append(cleanupFuncs, func() {
		log.Infof(ctx, "createCtr: deleting container %s from storage", ctr.ID())
		err2 := s.StorageRuntimeServer().DeleteContainer(ctr.ID())
		if err2 != nil {
			log.Warnf(ctx, "Failed to cleanup container storage: %v", err2)
		}
	})

	s.addContainer(newContainer)
	cleanupFuncs = append(cleanupFuncs, func() {
		log.Infof(ctx, "createCtr: removing container %s", newContainer.ID())
		s.removeContainer(newContainer)
	})

	if err := s.CtrIDIndex().Add(ctr.ID()); err != nil {
		return nil, err
	}
	cleanupFuncs = append(cleanupFuncs, func() {
		log.Infof(ctx, "createCtr: deleting container ID %s from idIndex", ctr.ID())
		if err := s.CtrIDIndex().Delete(ctr.ID()); err != nil {
			log.Warnf(ctx, "couldn't delete ctr id %s from idIndex", ctr.ID())
		}
	})

	mappings, err := s.getSandboxIDMappings(sb)
	if err != nil {
		return nil, err
	}

	if err := s.createContainerPlatform(ctx, newContainer, sb.CgroupParent(), mappings); err != nil {
		return nil, err
	}
	cleanupFuncs = append(cleanupFuncs, func() {
		if retErr != nil {
			log.Infof(ctx, "createCtr: removing container ID %s from runtime", ctr.ID())
			if err := s.Runtime().DeleteContainer(ctx, newContainer); err != nil {
				log.Warnf(ctx, "failed to delete container in runtime %s: %v", ctr.ID(), err)
			}
		}
	})

	if err := s.ContainerStateToDisk(ctx, newContainer); err != nil {
		log.Warnf(ctx, "unable to write containers %s state to disk: %v", newContainer.ID(), err)
	}

	if isContextError(ctx.Err()) {
		if err := s.resourceStore.Put(ctr.Name(), newContainer, cleanupFuncs); err != nil {
			log.Errorf(ctx, "createCtr: failed to save progress of container %s: %v", newContainer.ID(), err)
		}
		log.Infof(ctx, "createCtr: context was either canceled or the deadline was exceeded: %v", ctx.Err())
		return nil, ctx.Err()
	}

	newContainer.SetCreated()

	log.Infof(ctx, "Created container %s: %s", newContainer.ID(), newContainer.Description())
	return &types.CreateContainerResponse{
		ContainerID: ctr.ID(),
	}, nil
}
