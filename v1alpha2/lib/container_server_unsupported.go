// +build !linux

package lib

import (
	"github.com/cri-o/cri-o/v1alpha2/lib/sandbox"
	"github.com/cri-o/cri-o/v1alpha2/oci"
	"github.com/pkg/errors"
)

func (c *ContainerServer) addSandboxPlatform(sb *sandbox.Sandbox) {
	// nothin' doin'
}

func (c *ContainerServer) removeSandboxPlatform(sb *sandbox.Sandbox) {
	// nothin' doin'
}
