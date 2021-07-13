package experimental

import (
	"context"

	"github.com/cri-o/cri-o/server/cri/types"
)

func (s *service) CheckpointContainer(ctx context.Context, req *CheckpointContainerRequest) (res *CheckpointContainerResponse, retErr error) {
	r := &types.CheckpointContainerRequest{
		ID: req.Id,
		Options: &types.CheckpointContainerOptions{
			LeaveRunning: req.Options.LeaveRunning,
			CommonOptions: &types.CheckpointRestoreOptions{
				Keep:           req.Options.CommonOptions.Keep,
				TCPEstablished: req.Options.CommonOptions.TcpEstablished,
				Archive:        req.Options.CommonOptions.Archive,
				Compression:    req.Options.CommonOptions.Compression,
			},
		},
	}

	err := s.server.CheckpointContainer(ctx, r)
	if err != nil {
		return nil, err
	}
	return &CheckpointContainerResponse{}, nil
}
