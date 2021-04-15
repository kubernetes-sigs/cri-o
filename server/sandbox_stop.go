package server

import (
	"github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/log"
	"golang.org/x/net/context"
	pb "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

// StopPodSandbox stops the sandbox. If there are any running containers in the
// sandbox, they should be force terminated.
func (s *Server) StopPodSandbox(ctx context.Context, req *pb.StopPodSandboxRequest) (*pb.StopPodSandboxResponse, error) {
	// platform dependent call
	log.Infof(ctx, "Stopping pod sandbox: %s", req.PodSandboxId)
	sb, err := s.getPodSandboxFromRequest(req.PodSandboxId)
	if err != nil {
		if err == sandbox.ErrIDEmpty {
			return nil, err
		}

		// If the sandbox isn't found we just return an empty response to adhere
		// the CRI interface which expects to not error out in not found
		// cases.
		log.Warnf(ctx, "could not get sandbox %s, it's probably been stopped already: %v", req.PodSandboxId, err)
		log.Debugf(ctx, "StopPodSandboxResponse %s", req.PodSandboxId)
		return &pb.StopPodSandboxResponse{}, nil
	}
	if err := s.stopPodSandbox(ctx, sb); err != nil {
		return nil, err
	}
	return &pb.StopPodSandboxResponse{}, nil
}

// stopAllPodSandboxes removes all pod sandboxes
func (s *Server) stopAllPodSandboxes(ctx context.Context) {
	log.Debugf(ctx, "stopAllPodSandboxes")
	for _, sb := range s.ContainerServer.ListSandboxes() {
		if err := s.stopPodSandbox(ctx, sb); err != nil {
			log.Warnf(ctx, "could not StopPodSandbox %s: %v", sb.ID(), err)
		}
	}
}
