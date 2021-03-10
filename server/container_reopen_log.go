package server

import (
	"github.com/cri-o/cri-o/server/cri/types"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
)

// ReopenContainerLog reopens the containers log file
func (s *Server) ReopenContainerLog(ctx context.Context, req *types.ReopenContainerLogRequest) error {
	c, err := s.GetContainerFromShortID(req.ContainerID)
	if err != nil {
		return errors.Wrapf(err, "could not find container %s", req.ContainerID)
	}

	if err := c.IsAlive(); err != nil {
		return errors.Errorf("container is not created or running: %v", err)
	}

	if err := s.ContainerServer.Runtime().ReopenContainerLog(c); err != nil {
		return err
	}
	return nil
}
