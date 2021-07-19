package server

import (
	"fmt"

	"github.com/cri-o/cri-o/internal/log"
	"github.com/cri-o/cri-o/internal/runtimehandlerhooks"
	"github.com/cri-o/cri-o/server/cri/types"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// StopContainer stops a running container with a grace period (i.e., timeout).
func (s *Server) StopContainer(ctx context.Context, req *types.StopContainerRequest) error {
	log.Infof(ctx, "Stopping container: %s (timeout: %ds)", req.ContainerID, req.Timeout)
	c, err := s.GetContainerFromShortID(req.ContainerID)
	if err != nil {
		return status.Errorf(codes.NotFound, "could not find container %q: %v", req.ContainerID, err)
	}

	sandbox := s.getSandbox(c.Sandbox())
	hooks, err := runtimehandlerhooks.GetRuntimeHandlerHooks(&s.config, sandbox.Annotations())
	if err != nil {
		return fmt.Errorf("failed to get runtime handler %q hooks", sandbox.RuntimeHandler())
	}

	if hooks != nil {
		if err := hooks.PreStop(ctx, c, sandbox); err != nil {
			return fmt.Errorf("failed to run pre-stop hook for container %q: %v", c.ID(), err)
		}
	}

	if err := s.ContainerServer.StopContainer(ctx, c, req.Timeout); err != nil {
		return err
	}

	log.Infof(ctx, "Stopped container %s: %s", c.ID(), c.Description())
	return nil
}
