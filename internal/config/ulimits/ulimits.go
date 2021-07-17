package ulimits

import (
	"fmt"
	"strings"

	units "github.com/docker/go-units"
)

type Ulimit struct {
	Name string
	Hard uint64
	Soft uint64
}

type Config struct {
	ulimits []Ulimit
}

func New() *Config {
	return &Config{
		ulimits: make([]Ulimit, 0),
	}
}

func (c *Config) LoadUlimits(ulimits []string) error {
	// Process and initialize ulimits at cri-o start up, so crio fails early if
	// its misconfigured. After this, we can always refer to config.Ulimits() to get
	// the configured Ulimits
	for _, u := range ulimits {
		ul, err := units.ParseUlimit(u)
		if err != nil {
			return fmt.Errorf("unrecognized ulimit %s: %v", u, err)
		}
		rl, err := ul.GetRlimit()
		if err != nil {
			return err
		}
		// This isn't ideal, but it's the runtime-tools interface
		c.ulimits = append(c.ulimits, Ulimit{
			Name: "RLIMIT_" + strings.ToUpper(ul.Name),
			Hard: rl.Hard,
			Soft: rl.Soft,
		})
	}
	return nil
}

func (c *Config) Ulimits() []Ulimit {
	return c.ulimits
}
