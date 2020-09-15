package server

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"

	metadata "github.com/checkpoint-restore/checkpointctl/lib"
	"github.com/containers/podman/v3/libpod"
	"github.com/containers/podman/v3/pkg/annotations"
	"github.com/containers/podman/v3/pkg/errorhandling"
	"github.com/containers/storage/pkg/archive"
	"github.com/cri-o/cri-o/internal/lib"
	"github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/log"
	"github.com/cri-o/cri-o/pkg/container"
	"github.com/cri-o/cri-o/server/cri/types"
	spec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	pb "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

// RestoreContainer restores a container
func (s *Server) RestoreContainer(ctx context.Context, req *types.RestoreContainerRequest) (containerID string, err error) {
	if !s.config.RuntimeConfig.CheckpointRestore() {
		return "", fmt.Errorf("checkpoint/restore support not available")
	}
	// This is the place at which the restore request enters CRI-O.
	// Depending on the parameters the restore works in different ways:

	// # crictl restore ID
	// This is the most simple restore. The checkpoint was not exported
	// to a tar archive and the checkpoint is located at Dir()/checkpoint.
	// This relies on the original Pod, out of which the container was
	// checkpointed, to still exist as it will fail if the original Pod
	// no longer exists.

	// # crictl restore --pod=podID ID
	// This tries to restore a non-exported checkpoint into another pod.
	// The checkpointed container has to be stopped.
	// Possible scenario: checkpoint container out of Pod, reboot, restore
	// checkpointed container in newly created Pod after reboot.

	// Checkpoint a container and export it using
	// # crictl checkpoint --export=/tmp/cp.tar ID
	// # reboot
	// # crictl runp pod.json # to create new pod
	// # crictl restore --import=/tmp/cp.tar --pod=podID
	// This enables rebooting of a system without losing the state of a container
	var ctr string
	if req.Options.CommonOptions.Archive != "" {
		ctr, err = s.CRImportCheckpoint(ctx, req.Options.CommonOptions.Archive, req.Options.PodSandboxID)
		logrus.Debugf("Found ctr %s", ctr)
	} else {
		ctr = req.ID
		_, err = s.GetContainerFromShortID(ctr)
		log.Infof(ctx, "Restoring container: %s", ctr)
	}
	if err != nil {
		return "", errors.Wrapf(err, "failed to find container %s", ctr)
	}

	opts := &lib.ContainerCheckpointRestoreOptions{
		Container: ctr,
		Pod:       req.Options.PodSandboxID,
		ContainerCheckpointOptions: libpod.ContainerCheckpointOptions{
			TargetFile: req.Options.CommonOptions.Archive,
			Keep:       req.Options.CommonOptions.Keep,
		},
	}

	ctr, err = s.ContainerServer.ContainerRestore(ctx, opts)
	if err != nil {
		return "", err
	}

	return ctr, nil
}

// also taken from Podman
func (s *Server) CRImportCheckpoint(ctx context.Context, input, sbID string) (ctrID string, retErr error) {
	// First get the container definition from the
	// tarball to a temporary directory
	archiveFile, err := os.Open(input)
	if err != nil {
		return "", errors.Wrapf(err, "Failed to open checkpoint archive %s for import", input)
	}
	defer errorhandling.CloseQuiet(archiveFile)
	options := &archive.TarOptions{
		// Here we only need the files config.dump and spec.dump
		ExcludePatterns: []string{
			"artifacts",
			"ctr.log",
			metadata.RootFsDiffTar,
			metadata.NetworkStatusFile,
			metadata.DeletedFilesFile,
			metadata.CheckpointDirectory,
		},
	}
	dir, err := ioutil.TempDir("", "checkpoint")
	if err != nil {
		return "", err
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			logrus.Errorf("Could not recursively remove %s: %q", dir, err)
		}
	}()
	err = archive.Untar(archiveFile, dir, options)
	if err != nil {
		return "", errors.Wrapf(err, "Unpacking of checkpoint archive %s failed", input)
	}
	logrus.Debugf("Unpacked checkpoint in %s", dir)

	// Load spec.dump from temporary directory
	dumpSpec := new(spec.Spec)
	if _, err := metadata.ReadJSONFile(dumpSpec, dir, metadata.SpecDumpFile); err != nil {
		return "", errors.Wrapf(err, "Failed to read %q", metadata.SpecDumpFile)
	}

	// Load config.dump from temporary directory
	config := new(lib.ContainerConfig)
	if _, err := metadata.ReadJSONFile(config, dir, metadata.ConfigDumpFile); err != nil {
		return "", errors.Wrapf(err, "Failed to read %q", metadata.ConfigDumpFile)
	}

	if sbID == "" {
		// restore into previous sandbox
		sbID = dumpSpec.Annotations[annotations.SandboxID]
		ctrID = config.ID
	} else {
		ctrID = ""
	}

	ctrMetadata := pb.ContainerMetadata{}
	originalAnnotations := make(map[string]string)
	originalLabels := make(map[string]string)

	if dumpSpec.Annotations[annotations.ContainerManager] == "libpod" {
		// This is an import from Podman
		ctrMetadata.Name = config.Name
		ctrMetadata.Attempt = 0
	} else {
		if err := json.Unmarshal([]byte(dumpSpec.Annotations[annotations.Metadata]), &ctrMetadata); err != nil {
			return "", errors.Wrapf(err, "Failed to read %q", annotations.Metadata)
		}

		if err := json.Unmarshal([]byte(dumpSpec.Annotations[annotations.Annotations]), &originalAnnotations); err != nil {
			return "", errors.Wrapf(err, "Failed to read %q", annotations.Annotations)
		}

		if err := json.Unmarshal([]byte(dumpSpec.Annotations[annotations.Labels]), &originalLabels); err != nil {
			return "", errors.Wrapf(err, "Failed to read %q", annotations.Labels)
		}
	}

	s.updateLock.RLock()
	defer s.updateLock.RUnlock()
	sb, err := s.getPodSandboxFromRequest(sbID)
	if err != nil {
		if err == sandbox.ErrIDEmpty {
			return "", err
		}
		return "", errors.Wrapf(err, "specified sandbox not found: %s", sbID)
	}

	stopMutex := sb.StopMutex()
	stopMutex.RLock()
	defer stopMutex.RUnlock()
	if sb.Stopped() {
		return "", fmt.Errorf("CreateContainer failed as the sandbox was stopped: %s", sb.ID())
	}

	ctr, err := container.New()
	if err != nil {
		return "", errors.Wrap(err, "failed to create container")
	}

	containerConfig := &types.ContainerConfig{
		Metadata: &types.ContainerMetadata{
			Name:    ctrMetadata.Name,
			Attempt: ctrMetadata.Attempt,
		},
		Image: &types.ImageSpec{Image: config.RootfsImageName},
		Linux: &types.LinuxContainerConfig{
			Resources:       &types.LinuxContainerResources{},
			SecurityContext: types.NewLinuxContainerSecurityContext(),
		},
		Annotations: originalAnnotations,
		Labels:      originalLabels,
	}

	ignoreMounts := map[string]bool{
		"/proc":            true,
		"/dev":             true,
		"/dev/pts":         true,
		"/dev/mqueue":      true,
		"/sys":             true,
		"/sys/fs/cgroup":   true,
		"/dev/shm":         true,
		"/etc/resolv.conf": true,
		"/etc/hostname":    true,
		"/run/secrets":     true,
	}

	for _, m := range dumpSpec.Mounts {
		if ignoreMounts[m.Destination] {
			continue
		}
		mount := &types.Mount{
			ContainerPath: m.Destination,
			HostPath:      m.Source,
		}

		for _, opt := range m.Options {
			switch opt {
			case "ro":
				mount.Readonly = true
			case "rprivate":
				mount.Propagation = types.MountPropagationPropagationPrivate
			case "rshared":
				mount.Propagation = types.MountPropagationPropagationBidirectional
			case "rslaved":
				mount.Propagation = types.MountPropagationPropagationHostToContainer
			}
		}

		logrus.Debugf("Adding mounts %#v", mount)
		containerConfig.Mounts = append(containerConfig.Mounts, mount)
	}
	sandboxConfig := &types.PodSandboxConfig{
		Metadata: &types.PodSandboxMetadata{
			Name:      sb.Metadata().Name,
			UID:       sb.Metadata().UID,
			Namespace: sb.Metadata().Namespace,
			Attempt:   sb.Metadata().Attempt,
		},
		Linux: types.NewLinuxPodSandboxConfig(),
	}

	if err := ctr.SetConfig(containerConfig, sandboxConfig); err != nil {
		return "", errors.Wrap(err, "setting container config")
	}

	if err := ctr.SetNameAndID(ctrID); err != nil {
		return "", errors.Wrap(err, "setting container name and ID")
	}

	if _, err = s.ReserveContainerName(ctr.ID(), ctr.Name()); err != nil {
		return "", errors.Wrap(err, "Kubelet may be retrying requests that are timing out in CRI-O due to system load")
	}

	defer func() {
		if retErr != nil {
			log.Infof(ctx, "CreateCtr: releasing container name %s", ctr.Name())
			s.ReleaseContainerName(ctr.Name())
		}
	}()
	ctr.SetRestore(true)

	newContainer, err := s.createSandboxContainer(ctx, ctr, sb)
	if err != nil {
		return "", err
	}
	defer func() {
		if retErr != nil {
			log.Infof(ctx, "CreateCtr: deleting container %s from storage", ctr.ID())
			err2 := s.StorageRuntimeServer().DeleteContainer(ctr.ID())
			if err2 != nil {
				log.Warnf(ctx, "Failed to cleanup container directory: %v", err2)
			}
		}
	}()

	s.addContainer(newContainer)

	defer func() {
		if retErr != nil {
			log.Infof(ctx, "CreateCtr: removing container %s", newContainer.ID())
			s.removeContainer(newContainer)
		}
	}()

	if err := s.CtrIDIndex().Add(ctr.ID()); err != nil {
		return "", err
	}
	defer func() {
		if retErr != nil {
			log.Infof(ctx, "CreateCtr: deleting container ID %s from idIndex", ctr.ID())
			if err := s.CtrIDIndex().Delete(ctr.ID()); err != nil {
				log.Warnf(ctx, "Couldn't delete ctr id %s from idIndex", ctr.ID())
			}
		}
	}()

	newContainer.SetCreated()

	if ctx.Err() == context.Canceled || ctx.Err() == context.DeadlineExceeded {
		log.Infof(ctx, "CreateCtr: context was either canceled or the deadline was exceeded: %v", ctx.Err())
		return "", ctx.Err()
	}
	return ctr.ID(), nil
}
