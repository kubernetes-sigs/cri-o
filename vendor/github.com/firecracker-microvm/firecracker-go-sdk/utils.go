package firecracker

import (
	"context"
	"os"
	"strconv"
	"time"
)

const (
	defaultAliveVMMCheckDur = 10 * time.Millisecond
)

// waitForAliveVMM will check for periodically to see if the firecracker VMM is
// alive. If the VMM takes too long in starting, an error signifying that will
// be returned.
func waitForAliveVMM(ctx context.Context, client *Client) error {
	t := time.NewTicker(defaultAliveVMMCheckDur)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if _, err := client.GetMachineConfiguration(); err == nil {
				return nil
			}
		}
	}
}

// envValueOrDefaultInt check if env value exists and returns it or returns default value
// provided as a second param to this function
func envValueOrDefaultInt(envName string, def int) int {
	envVal, err := strconv.Atoi(os.Getenv(envName))
	if envVal == 0 || err != nil {
		envVal = def
	}
	return envVal
}
