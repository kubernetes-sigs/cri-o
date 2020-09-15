package lib

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	metadata "github.com/checkpoint-restore/checkpointctl/lib"
	"github.com/containers/podman/v3/libpod"
	"github.com/containers/podman/v3/pkg/annotations"
	"github.com/containers/podman/v3/pkg/checkpoint/crutils"
	"github.com/containers/storage/pkg/archive"
	"github.com/cri-o/cri-o/internal/oci"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type ContainerCheckpointRestoreOptions struct {
	Container string
	Pod       string

	libpod.ContainerCheckpointOptions
}

// ContainerCheckpoint checkpoints a running container.
func (c *ContainerServer) ContainerCheckpoint(ctx context.Context, opts *ContainerCheckpointRestoreOptions) (string, error) {
	ctr, err := c.LookupContainer(opts.Container)
	if err != nil {
		return "", errors.Wrapf(err, "failed to find container %s", opts.Container)
	}

	configFile := filepath.Join(ctr.BundlePath(), "config.json")
	specgen, err := generate.NewFromFile(configFile)
	if err != nil {
		return "", errors.Wrapf(err, "Not able to read config for container %q", ctr.ID())
	}

	cStatus := ctr.State()
	if cStatus.Status != oci.ContainerStateRunning {
		return "", fmt.Errorf("container %s is not running", ctr.ID())
	}

	if opts.TargetFile != "" {
		if err := c.prepareCheckpointExport(ctr); err != nil {
			return "", errors.Wrapf(err, "failed to write config dumps for container %s", ctr.ID())
		}
	}

	if err := c.runtime.CheckpointContainer(ctx, ctr, specgen.Config, opts.KeepRunning); err != nil {
		return "", errors.Wrapf(err, "failed to checkpoint container %s", ctr.ID())
	}
	if opts.TargetFile != "" {
		if err := c.exportCheckpoint(ctr, specgen.Config, opts.TargetFile); err != nil {
			return "", errors.Wrapf(err, "failed to write file system changes of container %s", ctr.ID())
		}
	}
	if err := c.storageRuntimeServer.StopContainer(ctr.ID()); err != nil {
		return "", errors.Wrapf(err, "failed to unmount container %s", ctr.ID())
	}
	if err := c.ContainerStateToDisk(ctx, ctr); err != nil {
		logrus.Warnf("Unable to write containers %s state to disk: %v", ctr.ID(), err)
	}

	if !opts.Keep {
		cleanup := []string{
			"dump.log",
			"stats-dump",
			metadata.ConfigDumpFile,
			metadata.SpecDumpFile,
		}
		for _, del := range cleanup {
			file := filepath.Join(ctr.Dir(), del)
			if err := os.Remove(file); err != nil {
				logrus.Debugf("Unable to remove file %s", file)
			}
		}
	}

	return ctr.ID(), nil
}

// Copied from libpod/diff.go
var containerMounts = map[string]bool{
	"/dev":               true,
	"/etc/hostname":      true,
	"/etc/hosts":         true,
	"/etc/resolv.conf":   true,
	"/proc":              true,
	"/run":               true,
	"/run/.containerenv": true,
	"/run/secrets":       true,
	"/sys":               true,
}

// getDiff returns the file system differences
// Copied from libpod/diff.go and simplified for the checkpoint use case
func (c *ContainerServer) getDiff(id string) ([]archive.Change, error) {
	layerID, err := c.GetContainerTopLayerID(id)
	if err != nil {
		return nil, err
	}
	var rchanges []archive.Change
	changes, err := c.store.Changes("", layerID)
	if err == nil {
		for _, c := range changes {
			if containerMounts[c.Path] {
				continue
			}
			rchanges = append(rchanges, c)
		}
	}
	return rchanges, err
}

// To make the checkpoint/restore code use the same fields as Podman:
type ContainerConfig struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	RootfsImageName string `json:"rootfsImageName,omitempty"`
}

// prepareCheckpointExport writes the config and spec to
// JSON files for later export
// Podman: libpod/container_internal.go
func (c *ContainerServer) prepareCheckpointExport(ctr *oci.Container) error {
	config := &ContainerConfig{
		ID:              ctr.ID(),
		Name:            ctr.Name(),
		RootfsImageName: ctr.ImageName(),
	}

	if _, err := metadata.WriteJSONFile(config, ctr.Dir(), metadata.ConfigDumpFile); err != nil {
		return err
	}

	// save spec
	jsonPath := filepath.Join(ctr.BundlePath(), "config.json")
	g, err := generate.NewFromFile(jsonPath)
	if err != nil {
		return errors.Wrapf(err, "generating spec for container %q failed", ctr.ID())
	}
	if _, err := metadata.WriteJSONFile(g.Config, ctr.Dir(), metadata.SpecDumpFile); err != nil {
		return errors.Wrapf(err, "generating spec for container %q failed", ctr.ID())
	}

	return nil
}

func (c *ContainerServer) exportCheckpoint(ctr *oci.Container, specgen *rspec.Spec, export string) error {
	id := ctr.ID()
	dest := ctr.Dir()
	logrus.Debugf("Exporting checkpoint image of container %q to %q", id, dest)

	includeFiles := []string{
		"dump.log",
		metadata.CheckpointDirectory,
		metadata.ConfigDumpFile,
		metadata.SpecDumpFile,
	}

	// To correctly track deleted files, let's go through the output of 'podman diff'
	rootFsChanges, err := c.getDiff(id)
	if err != nil {
		return errors.Wrapf(err, "error exporting root file-system diff for %q", id)
	}
	mountPoint, err := c.StorageImageServer().GetStore().Mount(id, specgen.Linux.MountLabel)
	if err != nil {
		return errors.Wrapf(err, "Not able to get mountpoint for container %q", id)
	}
	addToTarFiles, err := crutils.CRCreateRootFsDiffTar(&rootFsChanges, mountPoint, dest)
	if err != nil {
		return err
	}

	// Put log file into checkpoint archive
	_, err = os.Stat(specgen.Annotations[annotations.LogPath])
	if err == nil {
		src, err := os.Open(specgen.Annotations[annotations.LogPath])
		if err != nil {
			return errors.Wrapf(err, "error opening log file %q", specgen.Annotations[annotations.LogPath])
		}
		defer src.Close()
		destLog, err := os.Create(filepath.Join(dest, annotations.LogPath))
		if err != nil {
			return errors.Wrapf(err, "error opening log file %q", filepath.Join(dest, annotations.LogPath))
		}
		defer destLog.Close()
		_, err = io.Copy(destLog, src)
		if err != nil {
			return errors.Wrapf(err, "copying log file to %q failed", filepath.Join(dest, annotations.LogPath))
		}
		addToTarFiles = append(addToTarFiles, annotations.LogPath)
	}

	includeFiles = append(includeFiles, addToTarFiles...)

	input, err := archive.TarWithOptions(ctr.Dir(), &archive.TarOptions{
		// This should be configurable via api.proti
		Compression:      archive.Uncompressed,
		IncludeSourceDir: true,
		IncludeFiles:     includeFiles,
	})
	if err != nil {
		return errors.Wrapf(err, "error reading checkpoint directory %q", id)
	}

	outFile, err := os.Create(export)
	if err != nil {
		return errors.Wrapf(err, "error creating checkpoint export file %q", export)
	}
	defer outFile.Close()

	// The resulting tar archive should not readable by everyone as it contains
	// every memory page of the checkpointed processes.
	if err := os.Chmod(export, 0o600); err != nil {
		return errors.Wrapf(err, "cannot chmod %q", dest)
	}

	_, err = io.Copy(outFile, input)
	if err != nil {
		return err
	}

	for _, file := range addToTarFiles {
		os.Remove(filepath.Join(dest, file))
	}

	return nil
}
