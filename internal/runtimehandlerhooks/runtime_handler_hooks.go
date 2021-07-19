package runtimehandlerhooks

import (
	"context"
	"strings"

	"github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/oci"
	crioann "github.com/cri-o/cri-o/pkg/annotations"
	libconfig "github.com/cri-o/cri-o/pkg/config"
)

type RuntimeHandlerHooks interface {
	PreStart(ctx context.Context, c *oci.Container, s *sandbox.Sandbox) error
	PreStop(ctx context.Context, c *oci.Container, s *sandbox.Sandbox) error
}

// GetRuntimeHandlerHooks returns RuntimeHandlerHooks implementation by the runtime handler name
func GetRuntimeHandlerHooks(config *libconfig.Config, annotations map[string]string) (RuntimeHandlerHooks, error) {
	if highPerformanceAnnotationsSpecified(annotations) {
		return &HighPerformanceHooks{config.IrqBalanceConfigFile}, nil
	}

	return nil, nil
}

func highPerformanceAnnotationsSpecified(annotations map[string]string) bool {
	for k := range annotations {
		if strings.HasPrefix(k, crioann.CPULoadBalancingAnnotation) ||
			strings.HasPrefix(k, crioann.CPUQuotaAnnotation) ||
			strings.HasPrefix(k, crioann.IRQLoadBalancingAnnotation) {
			return true
		}
	}
	return false
}
