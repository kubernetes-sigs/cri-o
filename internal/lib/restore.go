package lib

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	metadata "github.com/checkpoint-restore/checkpointctl/lib"
	"github.com/containers/podman/v3/pkg/checkpoint/crutils"
	"github.com/cri-o/cri-o/internal/oci"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

func (c *ContainerServer) restoreFileSystemChanges(ctr *oci.Container, mountPoint string) error {
	if err := crutils.CRApplyRootFsDiffTar(ctr.Dir(), mountPoint); err != nil {
		return err
	}

	if err := crutils.CRRemoveDeletedFiles(ctr.ID(), ctr.Dir(), mountPoint); err != nil {
		return err
	}
	return nil
}

// ContainerRestore restores a checkpointed container.
func (c *ContainerServer) ContainerRestore(ctx context.Context, opts *ContainerCheckpointRestoreOptions) (string, error) {
	var ctr *oci.Container
	var err error
	ctr, err = c.LookupContainer(opts.Container)
	if err != nil {
		return "", errors.Wrapf(err, "failed to find container %s", opts.Container)
	}

	cStatus := ctr.State()
	if cStatus.Status == oci.ContainerStateRunning {
		return "", fmt.Errorf("cannot restore running container %s", ctr.ID())
	}

	// Get config.json
	configFile := filepath.Join(ctr.Dir(), "config.json")
	specgen, err := generate.NewFromFile(configFile)
	if err != nil {
		return "", err
	}
	// During checkpointing the container is unmounted. This mounts the container again.
	mountPoint, err := c.StorageImageServer().GetStore().Mount(ctr.ID(), specgen.Config.Linux.MountLabel)
	if err != nil {
		logrus.Debugf("Failed to mount container %q: %v", ctr.ID(), err)
		return "", err
	}
	logrus.Debugf("Container mountpoint %v", mountPoint)
	logrus.Debugf("Sandbox %v", ctr.Sandbox())
	logrus.Debugf("Specgen.Config.Annotations[io.kubernetes.cri-o.SandboxID] %v", specgen.Config.Annotations["io.kubernetes.cri-o.SandboxID"])
	// If there was no podID specified this will restore the container
	// in its original sandbox
	if opts.Pod == "" {
		opts.Pod = ctr.Sandbox()
	}
	sb, err := c.LookupSandbox(opts.Pod)
	if err != nil {
		return "", err
	}
	ic := sb.InfraContainer()
	if ic == nil {
		return "", fmt.Errorf("infra container of sandbox %v not found", ic.Name())
	}
	infraConfigFile := filepath.Join(ic.BundlePath(), "config.json")
	specgen, err = generate.NewFromFile(infraConfigFile)
	if err != nil {
		return "", err
	}

	if opts.TargetFile != "" {
		if err := crutils.CRImportCheckpointWithoutConfig(ctr.Dir(), opts.TargetFile); err != nil {
			return "", err
		}
		if err := c.restoreFileSystemChanges(ctr, mountPoint); err != nil {
			return "", err
		}
	}

	if err := c.runtime.RestoreContainer(ctx, ctr, specgen.Config, ic.State().Pid, sb.CgroupParent()); err != nil {
		return "", errors.Wrapf(err, "failed to restore container %s", ctr.ID())
	}
	if err := c.ContainerStateToDisk(ctx, ctr); err != nil {
		logrus.Warnf("Unable to write containers %s state to disk: %v", ctr.ID(), err)
	}

	if !opts.Keep {
		// Delete all checkpoint related files. At this point, in theory, all files
		// should exist. Still ignoring errors for now as the container should be
		// restored and running. Not erroring out just because some cleanup operation
		// failed. Starting with the checkpoint directory
		err = os.RemoveAll(ctr.CheckpointPath())
		if err != nil {
			logrus.Debugf("Non-fatal: removal of checkpoint directory (%s) failed: %v", ctr.CheckpointPath(), err)
		}
		cleanup := [...]string{
			"restore.log",
			"dump.log",
			"stats-dump",
			"stats-restore",
			metadata.NetworkStatusFile,
			metadata.RootFsDiffTar,
			metadata.DeletedFilesFile,
		}
		for _, del := range cleanup {
			var file string
			if del == "restore.log" || del == "stats-restore" {
				// Checkpointing uses runc and it is possible to tell runc
				// the location of the log file using '--work-path'.
				// Restore goes through conmon and conmon does (not yet?)
				// expose runc's '--work-path' which means that temporary
				// restore files are put into BundlePath().
				file = filepath.Join(ctr.BundlePath(), del)
			} else {
				file = filepath.Join(ctr.Dir(), del)
			}
			err = os.Remove(file)
			if err != nil {
				logrus.Debugf("Non-fatal: removal of checkpoint file (%s) failed: %v", file, err)
			}
		}
	}

	return ctr.ID(), nil
}
