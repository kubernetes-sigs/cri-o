package server

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"

	metadata "github.com/checkpoint-restore/checkpointctl/lib"
	"github.com/containers/podman/v3/libpod"
	"github.com/containers/storage/pkg/archive"
	"github.com/cri-o/cri-o/internal/lib"
	"github.com/cri-o/cri-o/internal/log"
	"github.com/cri-o/cri-o/server/cri/types"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

// CheckpointContainer checkpoints a container
func (s *Server) CheckpointContainer(ctx context.Context, req *types.CheckpointContainerRequest) error {
	if !s.config.RuntimeConfig.CheckpointRestore() {
		return fmt.Errorf("checkpoint/restore support not available")
	}

	var opts []*lib.ContainerCheckpointRestoreOptions
	var podCheckpointDirectory string
	var checkpointedPodOptions metadata.CheckpointedPodOptions

	_, err := s.GetContainerFromShortID(req.ID)
	if err != nil {
		// Maybe the user specified a Pod
		sb, err := s.LookupSandbox(req.ID)
		if err != nil {
			return status.Errorf(codes.NotFound, "could not find container or pod %q: %v", req.ID, err)
		}
		if req.Options.CommonOptions.Archive == "" {
			return status.Errorf(codes.NotFound, "Pod checkpointing requires a destination file")
		}

		log.Infof(ctx, "Checkpointing pod: %s", req.ID)
		// Create a temporary directory
		podCheckpointDirectory, err = ioutil.TempDir("", "checkpoint")
		if err != nil {
			return err
		}
		sandboxConfig := pb.PodSandboxConfig{
			Metadata: &pb.PodSandboxMetadata{
				Name:      sb.Metadata().Name,
				Uid:       sb.Metadata().UID,
				Namespace: sb.Metadata().Namespace,
				Attempt:   sb.Metadata().Attempt,
			},
			Hostname:     sb.Hostname(),
			LogDirectory: sb.LogDir(),
		}
		var portMappings []*pb.PortMapping
		maps := sb.PortMappings()
		for _, portMap := range maps {
			pm := &pb.PortMapping{
				ContainerPort: portMap.ContainerPort,
				HostPort:      portMap.HostPort,
				HostIp:        portMap.HostIP,
			}
			switch portMap.Protocol {
			case "TCP":
				pm.Protocol = pb.Protocol_TCP
			case "UDP":
				pm.Protocol = pb.Protocol_UDP
			case "SCTP":
				pm.Protocol = pb.Protocol_SCTP
			}

			portMappings = append(portMappings, pm)
		}
		sandboxConfig.PortMappings = portMappings
		if sb.DNSConfig() != nil {
			dnsConfig := &pb.DNSConfig{
				Servers:  sb.DNSConfig().Servers,
				Searches: sb.DNSConfig().Searches,
				Options:  sb.DNSConfig().Options,
			}
			sandboxConfig.DnsConfig = dnsConfig
		}
		if _, err := metadata.WriteJSONFile(sandboxConfig, podCheckpointDirectory, metadata.PodDumpFile); err != nil {
			return err
		}
		defer func() {
			if err := os.RemoveAll(podCheckpointDirectory); err != nil {
				logrus.Errorf("Could not recursively remove %s: %q", podCheckpointDirectory, err)
			}
		}()

		for _, ctr := range sb.Containers().List() {
			localOpts := &lib.ContainerCheckpointRestoreOptions{
				Container: ctr.ID(),
				ContainerCheckpointOptions: libpod.ContainerCheckpointOptions{
					TargetFile:  filepath.Join(podCheckpointDirectory, ctr.Name()+".tar"),
					Keep:        req.Options.CommonOptions.Keep,
					KeepRunning: req.Options.LeaveRunning,
				},
			}
			opts = append(opts, localOpts)
			checkpointedPodOptions.Containers = append(checkpointedPodOptions.Containers, ctr.Name())
		}
		if len(opts) == 0 {
			return status.Errorf(codes.NotFound, "No containers found in Pod %q", req.ID)
		}
		checkpointedPodOptions.Version = 1
		checkpointedPodOptions.MountLabel = sb.MountLabel()
		checkpointedPodOptions.ProcessLabel = sb.ProcessLabel()
	} else {
		log.Infof(ctx, "Checkpointing container: %s", req.ID)
		localOpts := &lib.ContainerCheckpointRestoreOptions{
			Container: req.ID,
			ContainerCheckpointOptions: libpod.ContainerCheckpointOptions{
				TargetFile:  req.Options.CommonOptions.Archive,
				Keep:        req.Options.CommonOptions.Keep,
				KeepRunning: req.Options.LeaveRunning,
			},
		}
		opts = append(opts, localOpts)
	}

	for _, opt := range opts {
		_, err = s.ContainerServer.ContainerCheckpoint(ctx, opt)
		if err != nil {
			return err
		}
	}

	if podCheckpointDirectory != "" {
		if podOptions, err := metadata.WriteJSONFile(checkpointedPodOptions, podCheckpointDirectory, metadata.PodOptionsFile); err != nil {
			return errors.Wrapf(err, "error creating checkpointedContainers list file %q", podOptions)
		}
		// It is a Pod checkpoint. Create the archive
		podTar, err := archive.TarWithOptions(podCheckpointDirectory, &archive.TarOptions{
			Compression:      archive.Compression(req.Options.CommonOptions.Compression),
			IncludeSourceDir: true,
		})
		if err != nil {
			return err
		}
		podTarFile, err := os.Create(req.Options.CommonOptions.Archive)
		if err != nil {
			return errors.Wrapf(err, "error creating pod checkpoint archive %q", req.Options.CommonOptions.Archive)
		}
		defer podTarFile.Close()
		_, err = io.Copy(podTarFile, podTar)
		if err != nil {
			return errors.Wrapf(err, "failed writing to pod tar archive %q", req.Options.CommonOptions.Archive)
		}
		// The resulting tar archive should not readable by everyone as it contains
		// every memory page of the checkpointed processes.
		if err := os.Chmod(req.Options.CommonOptions.Archive, 0o600); err != nil {
			return errors.Wrapf(err, "cannot chmod %q", req.Options.CommonOptions.Archive)
		}
		log.Infof(ctx, "Checkpointed pod: %s", req.ID)
	} else {
		log.Infof(ctx, "Checkpointed container: %s", req.ID)
	}

	return nil
}
