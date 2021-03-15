package server

import (
	"fmt"
	"io"

	"github.com/cri-o/cri-o/server/cri/types"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/tools/remotecommand"
)

// Attach prepares a streaming endpoint to attach to a running container.
func (s *Server) Attach(ctx context.Context, req *types.AttachRequest) (*types.AttachResponse, error) {
	resp, err := s.getAttach(req)
	if err != nil {
		return nil, fmt.Errorf("unable to prepare attach endpoint")
	}

	return resp, nil
}

// Attach endpoint for streaming.Runtime
func (s StreamService) Attach(containerID string, inputStream io.Reader, outputStream, errorStream io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	c, err := s.runtimeServer.GetContainerFromShortID(containerID)
	if err != nil {
		return status.Errorf(codes.NotFound, "could not find container %q: %v", containerID, err)
	}

	if err := c.IsAlive(); err != nil {
		return status.Errorf(codes.NotFound, "container is not created or running: %v", err)
	}

	return s.runtimeServer.Runtime().AttachContainer(c, inputStream, outputStream, errorStream, tty, resize)
}
