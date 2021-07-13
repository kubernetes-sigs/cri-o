// +build test
// All *_inject.go files are meant to be used by tests only. Purpose of this
// files is to provide a way to inject mocked data into the current setup.

package sandbox

import (
	"github.com/cri-o/cri-o/internal/hostport"
)

// SetPortMappings sets the PortMappings for the Sandbox
func (s *Sandbox) SetPortMappings(portMappings []*hostport.PortMapping) {
	s.portMappings = portMappings
}
