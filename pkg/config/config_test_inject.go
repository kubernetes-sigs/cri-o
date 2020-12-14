// +build test
// All *_inject.go files are meant to be used by tests only. Purpose of this
// files is to provide a way to inject mocked data into the current setup.

package config

import (
	"github.com/cri-o/ocicni/pkg/ocicni"
)

// SetCNIPlugin sets the network plugin for the Configuration. The function
// errors if a sane shutdown of the initially created network plugin failed.
func (c *NetworkConfig) SetCNIPlugin(plugin ocicni.CNIPlugin) error {
	if c.CNIPlugin() != nil {
		if err := c.CNIPlugin().Shutdown(); err != nil {
			return err
		}
	}
	c.cniPlugin = plugin
	return nil
}

// SetCheckpointRestore offers the possibility to turn on and
// turn off CheckpointRestore support for testing.
func (c *RuntimeConfig) SetCheckpointRestore(cr bool) {
	c.checkpointRestore = cr
}
