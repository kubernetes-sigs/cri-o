// +build linux

package node

import (
	"os/exec"
	"sync"

	"github.com/pkg/errors"
)

var (
	systemdHasCollectModeOnce sync.Once
	systemdHasCollectMode     bool
	systemdHasCollectModeErr  error
)

func SystemdHasCollectMode() bool {
	systemdHasCollectModeOnce.Do(func() {
		// This will show whether the currently running systemd supports CollectMode
		_, err := exec.Command("systemctl", "show", "-p", "CollectMode", "systemd").Output()
		if err != nil {
			systemdHasCollectModeErr = errors.Wrapf(err, "check systemd CollectMode")
			return
		}
		systemdHasCollectMode = true
	})
	return systemdHasCollectMode
}
