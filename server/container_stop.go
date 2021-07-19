package server

import (
	"fmt"

	"github.com/cri-o/cri-o/internal/log"
	"github.com/cri-o/cri-o/internal/runtimehandlerhooks"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

// StopContainer stops a running container with a grace period (i.e., timeout).
func (s *Server) StopContainer(ctx context.Context, req *pb.StopContainerRequest) (*pb.StopContainerResponse, error) {
	log.Infof(ctx, "Stopping container: %s", req.GetContainerId())
	// save container description to print
	c, err := s.GetContainerFromShortID(req.ContainerId)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "could not find container %q: %v", req.ContainerId, err)
	}

	sandbox := s.getSandbox(c.Sandbox())
	hooks := runtimehandlerhooks.GetRuntimeHandlerHooks(sandbox.RuntimeHandler())

	if hooks != nil {
		if err := hooks.PreStop(ctx, c, sandbox); err != nil {
			return nil, fmt.Errorf("failed to run pre-stop hook for container %q: %v", c.ID(), err)
		}
	}

	if err := s.ContainerServer.StopContainer(ctx, c, req.Timeout); err != nil {
		return nil, err
	}

	log.Infof(ctx, "Stopped container %s: %s", c.ID(), c.Description())
	return &pb.StopContainerResponse{}, nil
}
