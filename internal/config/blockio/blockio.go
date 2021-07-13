package blockio

import (
	"io/ioutil"
	"path/filepath"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"sigs.k8s.io/yaml"

	"github.com/intel/goresctrl/pkg/blockio"
)

type Config struct {
	enabled bool
	config  *blockio.Config
}

// New creates a new blockio config instance
func New() *Config {
	c := &Config{
		config: &blockio.Config{},
	}
	return c
}

// Enabled returns true if blockio is enabled in the system
func (c *Config) Enabled() bool {
	return c.enabled
}

// Load loads and validates blockio config
func (c *Config) Load(path string) error {
	c.enabled = false

	if path == "" {
		logrus.Info("No blockio config file specified, blockio not configured")
		return nil
	}

	data, err := ioutil.ReadFile(filepath.Clean(path))
	if err != nil {
		return errors.Wrap(err, "reading blockio config file failed")
	}

	tmpCfg := &blockio.Config{}
	if err = yaml.Unmarshal(data, &tmpCfg); err != nil {
		return errors.Wrap(err, "parsing blockio config failed")
	}

	if err := blockio.SetConfig(tmpCfg, true); err != nil {
		return errors.Wrap(err, "configuring blockio failed")
	}

	logrus.Infof("blockio config successfully loaded from %q", path)
	c.config = tmpCfg
	c.enabled = true
	return nil
}
