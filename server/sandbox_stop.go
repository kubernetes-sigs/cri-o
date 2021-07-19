package server

import (
	"fmt"

	"github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/log"
	"github.com/cri-o/cri-o/server/cri/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/net/context"
)

// StopPodSandbox stops the sandbox. If there are any running containers in the
// sandbox, they should be force terminated.
func (s *Server) StopPodSandbox(ctx context.Context, req *types.StopPodSandboxRequest) error {
	// platform dependent call
	log.Infof(ctx, "Stopping pod sandbox: %s", req.PodSandboxID)
	tracer := otel.GetTracerProvider().Tracer(s.tracerName)
	var span trace.Span
	ctx, span = tracer.Start(ctx, "stop-pod-sandbox")
	defer span.End()
	sb, err := s.getPodSandboxFromRequest(req.PodSandboxID)
	if err != nil {
		if err == sandbox.ErrIDEmpty {
			return err
		}
		if err == errSandboxNotCreated {
			return fmt.Errorf("StopPodSandbox failed as the sandbox is not created: %s", req.PodSandboxID)
		}

		// If the sandbox isn't found we just return an empty response to adhere
		// the CRI interface which expects to not error out in not found
		// cases.

		log.Warnf(ctx, "Could not get sandbox %s, it's probably been stopped already: %v", req.PodSandboxID, err)
		log.Debugf(ctx, "StopPodSandboxResponse %s", req.PodSandboxID)
		return nil
	}
	return s.stopPodSandbox(ctx, sb)
}

// stopAllPodSandboxes removes all pod sandboxes
func (s *Server) stopAllPodSandboxes(ctx context.Context) {
	log.Debugf(ctx, "StopAllPodSandboxes")
	for _, sb := range s.ContainerServer.ListSandboxes() {
		if err := s.stopPodSandbox(ctx, sb); err != nil {
			log.Warnf(ctx, "Could not StopPodSandbox %s: %v", sb.ID(), err)
		}
	}
}
