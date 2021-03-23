package server

import (
	"context"
	"path/filepath"

	"github.com/cri-o/cri-o/internal/log"
	oci "github.com/cri-o/cri-o/internal/oci"
	"github.com/cri-o/cri-o/server/cri/types"
	"github.com/pkg/errors"
)

// ContainerStats returns stats of the container. If the container does not
// exist, the call returns an error.
func (s *Server) ContainerStats(ctx context.Context, req *types.ContainerStatsRequest) (*types.ContainerStatsResponse, error) {
	container, err := s.GetContainerFromShortID(req.ContainerID)
	if err != nil {
		return nil, err
	}
	sb := s.GetSandbox(container.Sandbox())
	if sb == nil {
		return nil, errors.Errorf("unable to get stats for container %s: sandbox %s not found", container.ID(), container.Sandbox())
	}
	cgroup := sb.CgroupParent()

	stats, err := s.Runtime().ContainerStats(ctx, container, cgroup)
	if err != nil {
		return nil, err
	}

	return &types.ContainerStatsResponse{Stats: s.buildContainerStats(ctx, stats, container)}, nil
}

// buildContainerStats takes stats directly from the container, and attempts to inject the filesystem
// usage of the container.
// This is not taken care of by the container because we access information on the server level (storage driver).
func (s *Server) buildContainerStats(ctx context.Context, stats *oci.ContainerStats, container *oci.Container) *types.ContainerStats {
	writableLayer, err := s.writableLayerForContainer(stats, container)
	if err != nil {
		log.Warnf(ctx, "%v", err)
	}
	return &types.ContainerStats{
		Attributes: &types.ContainerAttributes{
			ID: container.ID(),
			Metadata: &types.ContainerMetadata{
				Name:    container.Metadata().Name,
				Attempt: container.Metadata().Attempt,
			},
			Labels:      container.Labels(),
			Annotations: container.Annotations(),
		},
		CPU: &types.CPUUsage{
			Timestamp:            stats.SystemNano,
			UsageCoreNanoSeconds: &types.UInt64Value{Value: stats.CPUNano},
		},
		Memory: &types.MemoryUsage{
			Timestamp:       stats.SystemNano,
			WorkingSetBytes: &types.UInt64Value{Value: stats.WorkingSetBytes},
		},
		WritableLayer: writableLayer,
	}
}

func (s *Server) writableLayerForContainer(stats *oci.ContainerStats, container *oci.Container) (*types.FilesystemUsage, error) {
	writableLayer := &types.FilesystemUsage{
		Timestamp: stats.SystemNano,
		FsID:      &types.FilesystemIdentifier{Mountpoint: container.MountPoint()},
	}
	driver, err := s.Store().GraphDriver()
	if err != nil {
		return writableLayer, errors.Wrapf(err, "unable to get graph driver for disk usage for container %s", container.ID())
	}

	id := filepath.Base(filepath.Dir(container.MountPoint()))
	usage, err := driver.ReadWriteDiskUsage(id)
	if err != nil {
		return writableLayer, errors.Wrapf(err, "unable to get disk usage for container %s", container.ID())
	}
	writableLayer.UsedBytes = &types.UInt64Value{Value: uint64(usage.Size)}
	writableLayer.InodesUsed = &types.UInt64Value{Value: uint64(usage.InodeCount)}
	return writableLayer, nil
}
